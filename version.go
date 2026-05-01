package starling

// Version is the semantic version of the Starling library and bundled
// CLI binaries. Bumped per release per the policy documented in
// CHANGELOG.md and the README.
//
// Build tooling can override this via -ldflags="-X
// github.com/jerkeyray/starling.Version=vX.Y.Z" so dev builds report
// the underlying tag (or "dev" when off-tag); the constant here is
// the source of truth shipped with the tagged release.
var Version = "v0.1.0-beta.2-dev"
