# Derived objects

Every macro and view of the gold schema lives here, not in `migrations/`.

- One home per object. Each file holds one family. Each object is one
  plain `CREATE MACRO` or `CREATE VIEW`, with its comment block above it.
- Order. The files run in name order, each top to bottom. An object names
  only objects above it. A test checks this by a name scan.
- A statement ends with `;` at the end of a line. No other code line ends
  with `;`.
- `LOCK` holds one line `<version> <sha256>` per derived version. The hash
  is of the files' bytes, concatenated in name order. The version takes
  the next free number on the migrations' number line.
- From 180 on, the migrations and the derived versions together use each
  number once, with no gap: a test holds this.
- `Migrate` applies the set whole when the database is below the version
  or carries another hash: it drops every macro and view and runs the
  files. It never touches a database above the version.
- To change a macro: edit its body here, take the next number, and append
  `<number> <hash>` to `LOCK`. A test prints the hash when it is missing.
- A serving table is an entry in `servingTables`
  (`serving_materialize.go`). The apply gives each one its macro's
  columns and re-creates it empty where they moved. A new entry takes the
  next number too.
- A migration holds tables and data moves only. It creates and reads no
  macro or view.
