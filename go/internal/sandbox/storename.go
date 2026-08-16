package sandbox

import "strings"

// StoreName makes a store-object name out of a filename.
//
// Nix restricts store path names to [A-Za-z0-9+._?=-] and forbids a
// leading dot; anything else is rejected outright:
//
//	error: path 'ir4kjkq…-qcom,wcd9335.h' is not a valid store path:
//	name 'qcom,wcd9335.h' contains illegal character ','
//
// Shared staging names each store object after the file it holds, which
// puts arbitrary source filenames through that check. The kernel is full
// of device-tree binding headers with commas in the name —
// include/dt-bindings/sound/qcom,wcd9335.h and ~1600 siblings — so this
// is not an edge case; it killed a full-kernel run 6,473 compiles in,
// at sound/soc/codecs.
//
// Renaming is safe because the name is cosmetic here. A staged farm
// symlinks <farm>/<original/relative/path> at the store object, so the
// compiler resolves the include by the path in the farm and never sees
// the object's own name. The object's identity comes from its content
// hash, not its name.
//
// Two different filenames can map to the same name (a,b.h and a_b.h).
// That is fine: they still hash separately unless their CONTENT is also
// identical, in which case sharing one object is exactly right.
func StoreName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '-', r == '.', r == '_', r == '?', r == '=':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	// A leading dot is rejected, and "." / ".." are not names at all.
	s = strings.TrimLeft(s, ".")
	// Nix caps names at 211 chars. Trim from the FRONT so the extension
	// survives — it is the part that carries meaning when reading a log.
	const max = 200
	if len(s) > max {
		s = s[len(s)-max:]
	}
	if s == "" {
		s = "unnamed"
	}
	return s
}
