#include <stdio.h>

extern int foo(void);
extern int bar(void);

int main(void) {
    int ok = foo() == 42 && bar() == 43;
    printf("thin-archive: %s\n", ok ? "ok" : "FAIL");
    return ok ? 0 : 1;
}
