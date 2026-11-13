package silver_test

// The import registers silvertest's -update-canonical flag in this
// package's test binary too, so a run over ./internal/silver/... with
// the flag rewrites every golden file and fails no package.
import _ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
