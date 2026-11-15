package spending_test

// The silver adapters register the sign pins (canonical.CashSignedKinds)
// from their init. Importing them here, in the test binary only, makes the
// pass tests stamp the real pins.
import _ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
