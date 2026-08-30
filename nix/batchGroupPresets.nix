# Starting-point batchGroups declarations. NOT guaranteed correct or
# complete for any specific package — this is prototype-scope
# classification/logging only (see go/internal/batch's own package
# docstring): nixgg still submits one derivation per TU regardless of
# which group a compile matches, so an over- or under-inclusive
# pattern here has NO correctness consequence yet, unlike
# configureSrcFilterPresets.nix's own patterns (which DO gate what's
# visible to configure and can silently produce a stale build).
#
# The judgment call these presets encode is "this subtree is rarely
# edited relative to how often the project rebuilds" — vendored/
# third-party dependency trees are the common case. A project author
# (or repo-inspection tooling computing actual edit frequency from git
# history) is expected to verify and likely extend this list per
# project, the same way configureSrcFilterPresets' own docstring
# describes for its patterns.
#
# Usage:
#
#   mkNixggBuild {
#     # ...
#     batchGroups = [
#       { name = "vendor"; patterns = batchGroupPresets.vendorDeps; }
#     ];
#   }
{
  # Common vendored-dependency directory names, matched at any depth
  # via internal/batch's "**" extension (NOT plain filepath.Match
  # syntax — see matchPath's own docstring). Covers redis's deps/,
  # autotools-style third_party/ and vendor/, and Go's own vendor/
  # convention, for C/C++ source files.
  vendorDeps = [
    "deps/**/*.c"
    "deps/**/*.cc"
    "deps/**/*.cpp"
    "third_party/**/*.c"
    "third_party/**/*.cc"
    "third_party/**/*.cpp"
    "vendor/**/*.c"
    "vendor/**/*.cc"
    "vendor/**/*.cpp"
  ];
}
