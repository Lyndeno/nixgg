CC ?= cc
OBJS := foo.o bar.o

thin-archive: libthin.a main.o
	$(CC) main.o libthin.a -o thin-archive

libthin.a: $(OBJS)
	ar csrDT libthin.a $(OBJS)

%.o: %.c
	$(CC) -O2 -c $< -o $@

clean:
	rm -f $(OBJS) main.o libthin.a thin-archive

.PHONY: clean
