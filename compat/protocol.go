package compat

// ProtocolSchemaDigest pins the experimental 0.162.0 JSON schema corpus used
// by this bridge. Generated locally with app-server generate-json-schema; hash
// SHA256 of a compact JSON object mapping sorted relative POSIX names to each
// file's SHA256. It is a build compatibility manifest, not a live schema probe.
// The same corpus includes Thread.section, threadSection/list and thread/list's
// sectionId, sourceKinds, updated_at and section_position filters/orderings.
// codex-pinning.json records their use and the separate real App evidence.
const ProtocolSchemaDigest = "4248c44d68df2d0d55be53b15fffbb24d8c8cf41cb9993a1f79d5d03d3d2b5da"
