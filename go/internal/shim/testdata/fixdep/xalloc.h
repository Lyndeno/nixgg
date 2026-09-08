#ifndef XALLOC_H
#define XALLOC_H
#include <stdlib.h>
#include <stdio.h>
static inline void *xmalloc(size_t size) {
	void *p = malloc(size);
	if (!p) { fprintf(stderr, "xmalloc failed\n"); exit(1); }
	return p;
}
static inline void *xrealloc(void *p, size_t size) {
	void *n = realloc(p, size);
	if (!n) { fprintf(stderr, "xrealloc failed\n"); exit(1); }
	return n;
}
static inline void *xcalloc(size_t n, size_t size) {
	void *p = calloc(n, size);
	if (!p) { fprintf(stderr, "xcalloc failed\n"); exit(1); }
	return p;
}
#endif
