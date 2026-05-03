package kamatera

import "os"

// Thin wrappers used by client_test.go. Kept in a separate file so the test
// helper plumbing doesn't clutter the primary tests.

func lookupEnv(k string) (string, bool) { return os.LookupEnv(k) }
func setEnv(k, v string)                { _ = os.Setenv(k, v) }
func unsetEnv(k string)                 { _ = os.Unsetenv(k) }
