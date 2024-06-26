//go:build encore_app

package et

import (
	"context"

	"encore.dev/beta/auth"
	"encore.dev/storage/sqldb"
)

// OverrideAuthInfo overrides the auth information for the current request.
// Subsequent calls to auth.UserID and auth.Data() within the same request
// will return the given uid and data, and API calls made from the request
// will propagate the newly set user info.
//
// Passing in an empty string as the uid results in unsetting the auth information,
// causing future API calls to behave as if there was no authenticated user.
//
// If the application's auth handler returns custom auth data, two additional
// requirements exist. First, the data parameter passed to WithContext must be of
// the same type as the auth handler returns. Second, if the uid argument is not
// the empty string then data may not be nil. If these requirements are not met,
// API calls made with these options will not be made and will immediately return
// a client-side error.
//
// OverrideAuthInfo is not safe for concurrent use with code that invokes
// auth.UserID or auth.Data() within the same request.
func OverrideAuthInfo(uid auth.UID, data any) {
	Singleton.OverrideAuthInfo(uid, data)
}

// EnableServiceInstanceIsolation will causes all Service singletons to be isolated to each test
// from this test and on any of its sub-tests. (Calling this in a TestMain has the impact
// of isolating all tests in the package.)
//
// By default, Service singletons are shared across all tests and initialized on
// the first call to that service by any test, which results in faster tests as you
// are not re-initializing the service for each test, however if your service structs
// contain state that is not reset between tests, this can cause issues. In that case,
// you can call this function to isolate the services for the impacted tests.
func EnableServiceInstanceIsolation() {
	Singleton.testMgr.SetIsolatedServices(true)
}

//publicapigen:keep
type stringLiteral string

// NewTestDatabase creates a new, fresh database for the database with the given name.
// The database name must be a database known to Encore (via `sqldb.NewDatabase`),
// otherwise it reports an error.
//
// The new database is cloned from a template database that has had all migrations applied to it,
// but excludes any of the changes applied to the given db.
//
// The returned database is isolated to the current test and any sub-tests,
// and is automatically dropped at the end of the test, and any
// open connections are automatically closed.
//
// The provided name must be a constant string literal (like "mydb").
func NewTestDatabase(ctx context.Context, name stringLiteral) (*sqldb.Database, error) {
	return Singleton.db.NewTestDatabase(ctx, string(name))
}
