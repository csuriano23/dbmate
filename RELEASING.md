# Releasing

The following steps should be followed to publish a new version of dbmate (requires write access to this repository).

1. Update [version.go](/pkg/dbmate/version.go) and [README.md](/README.md) with new version number ([example PR](https://github.com/csuriano23/dbmate-oracle/pull/4/files))
2. Create new release on GitHub project [releases page](https://github.com/csuriano23/dbmate-oracle/releases)
3. Travis CI will automatically publish release binaries to GitHub
