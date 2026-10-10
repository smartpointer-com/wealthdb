// Package lots is the lot engine. It replays a source's trades into
// open lots, a cost basis for every position row and realized lots for
// every sale, where the source states none of them (docs/LOTS.md).
//
// The package is pure, like internal/returns: a sorted stream of
// Events in, a Result out, no database. It also holds the Policy each
// source kind registers beside its adapter (RegisterPolicy), the method
// the `lots` config block puts in force, and the two readings of a
// missing cost basis the readers share (MissingBasis). The feed that
// reads gold into events and the writer that stores a Result live in
// internal/gold (lots_feed.go, lots_writer.go), which runs them as one
// pass (lots_pass.go).
//
// Conventions:
//
//   - All arithmetic is float64. Gold holds the amounts as DECIMAL; the
//     engine floats them at the feed and the writer rounds them back to
//     the column's scale.
//   - Times are Unix seconds; days are Unix days (seconds / 86400, UTC).
//   - Quantities are magnitudes. An event's direction is its kind.
package lots
