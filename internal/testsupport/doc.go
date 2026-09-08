// Package testsupport provides the database harness the integration tests use.
//
// Everything in it is behind the `integration` build tag, because it exists to
// truncate and reseed a real Postgres and nothing that runs in a normal `go
// test ./...` should be able to reach it. Run the tests that use it with:
//
//	make test-integration
//	# or: TEST_DATABASE_URL=... go test -p 1 -tags=integration ./...
//
// Store truncates every table, so the test binaries must not run against one
// database concurrently -- hence -p 1. Within a package Go already runs tests
// one at a time, so no test in this project calls t.Parallel.
package testsupport
