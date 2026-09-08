`fixdep.c` is a verbatim copy of the Linux kernel's
`scripts/basic/fixdep.c` (from torvalds/linux, author Kai
Germaschewski, copyright 2002), licensed GPL-2.0 — see the license
notice in the file itself. It is test-only fixture data: compiled
into a standalone throwaway binary by
`TestWriteSynthesizedDepfileSatisfiesRealFixdep` (in
`../../compile_test.go`) to validate that nixgg's synthesized `.d`
depfiles are indistinguishable from genuine `gcc -MD` output to the
real kernel tool that consumes them. It is never built into, or
distributed as part of, nixgg's own MIT-licensed binaries.

`xalloc.h` is a small local stub (not from upstream) providing the
`xmalloc`/`xrealloc`/`xcalloc` wrappers `fixdep.c` expects from the
kernel's own `tools/include/` — the only header dependency fixdep.c
has beyond the C standard library.
