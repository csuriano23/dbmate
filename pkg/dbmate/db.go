package dbmate

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/scanner"
	"time"
)

// DefaultMigrationsDir specifies default directory to find migration files
const DefaultMigrationsDir = "./db/migrations"

// DefaultSchemaFile specifies default location for schema.sql
const DefaultSchemaFile = "./db/schema.sql"

// DefaultWaitInterval specifies length of time between connection attempts
const DefaultWaitInterval = time.Second

// DefaultWaitTimeout specifies maximum time for connection attempts
const DefaultWaitTimeout = 60 * time.Second

const endOfStatement = ';'

// DB allows dbmate actions to be performed on a specified database
type DB struct {
	AutoDumpSchema bool
	DatabaseURL    *url.URL
	MigrationsDir  string
	SchemaFile     string
	WaitBefore     bool
	WaitInterval   time.Duration
	WaitTimeout    time.Duration
	NativeEngine   bool
}

// migrationFileRegexp pattern for valid migration files
var migrationFileRegexp = regexp.MustCompile(`^\d.*\.sql$`)

type statusResult struct {
	filename string
	applied  bool
}

// New initializes a new dbmate database
func New(databaseURL *url.URL) *DB {
	return &DB{
		AutoDumpSchema: true,
		DatabaseURL:    databaseURL,
		MigrationsDir:  DefaultMigrationsDir,
		SchemaFile:     DefaultSchemaFile,
		WaitBefore:     false,
		WaitInterval:   DefaultWaitInterval,
		WaitTimeout:    DefaultWaitTimeout,
		NativeEngine:   true,
	}
}

// GetDriver loads the required database driver
func (db *DB) GetDriver() (Driver, error) {
	return GetDriver(db.DatabaseURL.Scheme)
}

// Wait blocks until the database server is available. It does not verify that
// the specified database exists, only that the host is ready to accept connections.
func (db *DB) Wait() error {
	drv, err := db.GetDriver()
	if err != nil {
		return err
	}

	// attempt connection to database server
	err = drv.Ping(db.DatabaseURL)
	if err == nil {
		// connection successful
		return nil
	}

	fmt.Print("Waiting for database")
	for i := 0 * time.Second; i < db.WaitTimeout; i += db.WaitInterval {
		fmt.Print(".")
		time.Sleep(db.WaitInterval)

		// attempt connection to database server
		err = drv.Ping(db.DatabaseURL)
		if err == nil {
			// connection successful
			fmt.Print("\n")
			return nil
		}
	}

	// if we find outselves here, we could not connect within the timeout
	fmt.Print("\n")
	return fmt.Errorf("unable to connect to database: %s", err)
}

// CreateAndMigrate creates the database (if necessary) and runs migrations
func (db *DB) CreateAndMigrate() error {
	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, err := db.GetDriver()
	if err != nil {
		return err
	}

	// create database if it does not already exist
	// skip this step if we cannot determine status
	// (e.g. user does not have list database permission)
	exists, err := drv.DatabaseExists(db.DatabaseURL)
	if err == nil && !exists {
		if err := drv.CreateDatabase(db.DatabaseURL); err != nil {
			return err
		}
	}

	// migrate
	return db.Migrate()
}

// Create creates the current database
func (db *DB) Create() error {
	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, err := db.GetDriver()
	if err != nil {
		return err
	}

	return drv.CreateDatabase(db.DatabaseURL)
}

// Drop drops the current database (if it exists)
func (db *DB) Drop() error {
	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, err := db.GetDriver()
	if err != nil {
		return err
	}

	return drv.DropDatabase(db.DatabaseURL)
}

// DumpSchema writes the current database schema to a file
func (db *DB) DumpSchema() error {
	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, sqlDB, err := db.openDatabaseForMigration()
	if err != nil {
		return err
	}
	defer mustClose(sqlDB)

	schema, err := drv.DumpSchema(db.DatabaseURL, sqlDB)
	if err != nil {
		return err
	}

	fmt.Printf("Writing: %s\n", db.SchemaFile)

	// ensure schema directory exists
	if err = ensureDir(filepath.Dir(db.SchemaFile)); err != nil {
		return err
	}

	// write schema to file
	return ioutil.WriteFile(db.SchemaFile, schema, 0644)
}

const migrationTemplate = "-- migrate:up\n\n\n-- migrate:down\n\n"

// NewMigration creates a new migration file
func (db *DB) NewMigration(name string) error {
	// new migration name
	timestamp := time.Now().UTC().Format("20060102150405")
	if name == "" {
		return fmt.Errorf("please specify a name for the new migration")
	}
	name = fmt.Sprintf("%s_%s.sql", timestamp, name)

	// create migrations dir if missing
	if err := ensureDir(db.MigrationsDir); err != nil {
		return err
	}

	// check file does not already exist
	path := filepath.Join(db.MigrationsDir, name)
	fmt.Printf("Creating migration: %s\n", path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return fmt.Errorf("file already exists")
	}

	// write new migration
	file, err := os.Create(path)
	if err != nil {
		return err
	}

	defer mustClose(file)
	_, err = file.WriteString(migrationTemplate)
	return err
}

func doTransaction(db *sql.DB, txFunc func(Transaction) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if err := txFunc(tx); err != nil {
		if err1 := tx.Rollback(); err1 != nil {
			return err1
		}

		return err
	}

	return tx.Commit()
}

func (db *DB) openDatabaseForMigration() (Driver, *sql.DB, error) {
	drv, err := db.GetDriver()
	if err != nil {
		return nil, nil, err
	}

	sqlDB, err := drv.Open(db.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}

	if err := drv.CreateMigrationsTable(sqlDB); err != nil {
		mustClose(sqlDB)
		return nil, nil, err
	}

	return drv, sqlDB, nil
}

func parseStatements(script string) []string {
	var (
		s          scanner.Scanner
		statement  string
		statements []string
		parsedLast bool
	)

	s.Init(strings.NewReader(script))
	s.Whitespace ^= 1<<' ' | 1<<'\t' | 1<<'\n'
	s.IsIdentRune = func(ch rune, i int) bool {
		return false
	}
	s.Error = nil
	s.Mode ^= scanner.ScanChars

	for tok := s.Scan(); !parsedLast; tok = s.Scan() {
		if tok == scanner.EOF {
			parsedLast = true
		}

		switch tok {
		case endOfStatement, scanner.EOF:
			if strings.TrimSpace(statement) != "" {
				statements = append(statements, statement)
			}
			//restart
			statement = ""
		default:
			statement += s.TokenText()
		}
	}

	return statements
}

func executeScript(tx Transaction, script string, nativeEngine bool) error {
	var err error

	if nativeEngine {
		fmt.Println("Executing script on native engine")
	} else {
		fmt.Println("Executing script on DBMate engine")
	}

	if nativeEngine {
		_, err = tx.Exec(script)
		return err
	}

	for _, statement := range parseStatements(script) {
		_, err = tx.Exec(statement)
		if err != nil {
			return err
		}
	}

	return err
}

// Migrate migrates database to the latest version
func (db *DB) Migrate() error {
	files, err := findMigrationFiles(db.MigrationsDir, migrationFileRegexp)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return fmt.Errorf("no migration files found")
	}

	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, sqlDB, err := db.openDatabaseForMigration()
	if err != nil {
		return err
	}
	defer mustClose(sqlDB)

	useNative := db.NativeEngine && db.DatabaseURL.Scheme != "oracle"

	applied, err := drv.SelectMigrations(sqlDB, -1)
	if err != nil {
		return err
	}

	for _, filename := range files {
		ver := migrationVersion(filename)
		if ok := applied[ver]; ok {
			// migration already applied
			continue
		}

		fmt.Printf("Applying: %s\n", filename)

		up, _, err := parseMigration(filepath.Join(db.MigrationsDir, filename))
		if err != nil {
			return err
		}

		execMigration := func(tx Transaction) error {
			// run actual migration
			if err := executeScript(tx, up.Contents, useNative); err != nil {
				return err
			}

			// record migration
			return drv.InsertMigration(tx, ver)
		}

		if up.Options.Transaction() {
			// begin transaction
			err = doTransaction(sqlDB, execMigration)
		} else {
			// run outside of transaction
			err = execMigration(sqlDB)
		}

		if err != nil {
			return err
		}
	}

	// automatically update schema file, silence errors
	if db.AutoDumpSchema {
		_ = db.DumpSchema()
	}

	return nil
}

func findMigrationFiles(dir string, re *regexp.Regexp) ([]string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("could not find migrations directory `%s`", dir)
	}

	matches := []string{}
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		name := file.Name()
		if !re.MatchString(name) {
			continue
		}

		matches = append(matches, name)
	}

	sort.Strings(matches)

	return matches, nil
}

func findMigrationFile(dir string, ver string) (string, error) {
	if ver == "" {
		panic("migration version is required")
	}

	ver = regexp.QuoteMeta(ver)
	re := regexp.MustCompile(fmt.Sprintf(`^%s.*\.sql$`, ver))

	files, err := findMigrationFiles(dir, re)
	if err != nil {
		return "", err
	}

	if len(files) == 0 {
		return "", fmt.Errorf("can't find migration file: %s*.sql", ver)
	}

	return files[0], nil
}

func migrationVersion(filename string) string {
	return regexp.MustCompile(`^\d+`).FindString(filename)
}

// Rollback rolls back the most recent migration
func (db *DB) Rollback() error {
	if db.WaitBefore {
		err := db.Wait()
		if err != nil {
			return err
		}
	}

	drv, sqlDB, err := db.openDatabaseForMigration()
	if err != nil {
		return err
	}
	defer mustClose(sqlDB)

	useNative := db.NativeEngine && db.DatabaseURL.Scheme != "oracle"

	applied, err := drv.SelectMigrations(sqlDB, 1)
	if err != nil {
		return err
	}

	// grab most recent applied migration (applied has len=1)
	latest := ""
	for ver := range applied {
		latest = ver
	}
	if latest == "" {
		return fmt.Errorf("can't rollback: no migrations have been applied")
	}

	filename, err := findMigrationFile(db.MigrationsDir, latest)
	if err != nil {
		return err
	}

	fmt.Printf("Rolling back: %s\n", filename)

	_, down, err := parseMigration(filepath.Join(db.MigrationsDir, filename))
	if err != nil {
		return err
	}

	execMigration := func(tx Transaction) error {
		// rollback migration
		if err := executeScript(tx, down.Contents, useNative); err != nil {
			return err
		}

		// remove migration record
		return drv.DeleteMigration(tx, latest)
	}

	if down.Options.Transaction() {
		// begin transaction
		err = doTransaction(sqlDB, execMigration)
	} else {
		// run outside of transaction
		err = execMigration(sqlDB)
	}

	if err != nil {
		return err
	}

	// automatically update schema file, silence errors
	if db.AutoDumpSchema {
		_ = db.DumpSchema()
	}

	return nil
}

func checkMigrationsStatus(db *DB) ([]statusResult, error) {
	files, err := findMigrationFiles(db.MigrationsDir, migrationFileRegexp)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no migration files found")
	}

	drv, sqlDB, err := db.openDatabaseForMigration()
	if err != nil {
		return nil, err
	}
	defer mustClose(sqlDB)

	applied, err := drv.SelectMigrations(sqlDB, -1)
	if err != nil {
		return nil, err
	}

	var results []statusResult

	for _, filename := range files {
		ver := migrationVersion(filename)
		res := statusResult{filename: filename}
		if ok := applied[ver]; ok {
			res.applied = true
		} else {
			res.applied = false
		}

		results = append(results, res)
	}

	return results, nil
}

// Status shows the status of all migrations
func (db *DB) Status(quiet bool) (int, error) {
	results, err := checkMigrationsStatus(db)
	if err != nil {
		return -1, err
	}

	var totalApplied int
	var line string

	for _, res := range results {
		if res.applied {
			line = fmt.Sprintf("[X] %s", res.filename)
			totalApplied++
		} else {
			line = fmt.Sprintf("[ ] %s", res.filename)
		}
		if !quiet {
			fmt.Println(line)
		}
	}

	totalPending := len(results) - totalApplied
	if !quiet {
		fmt.Println()
		fmt.Printf("Applied: %d\n", totalApplied)
		fmt.Printf("Pending: %d\n", totalPending)
	}

	return totalPending, nil
}
