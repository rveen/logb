// The viewer is a separate module on purpose. The core library advertises a
// ~1000-line reader with near-zero dependencies (SPEC.md rule 4), and everyone
// who `go get`s github.com/rveen/logb downloads the whole module zip. Keeping
// the embedded frontend bundle under a nested go.mod excludes it from the
// parent module's file set, so the core stays as small as it claims to be.
module github.com/rveen/logb/viewer

go 1.25.6

require github.com/rveen/logb v0.0.0

// The core has no tagged release yet, so there is no version to require. Drop
// this and pin a real version once the parent is tagged; until then `go install
// .../viewer/cmd/logbview@latest` cannot work regardless.
replace github.com/rveen/logb => ../

require github.com/klauspost/compress v1.19.0 // indirect
