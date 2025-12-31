.PHONY: build install clean

build:
	go build -o ccc
	@if [ "$$(uname)" = "Darwin" ]; then \
		codesign -f -s - ccc 2>/dev/null || true; \
	fi

install: build
	mkdir -p ~/bin
	install -m 755 ccc ~/bin/ccc
	@echo "✅ Installed to ~/bin/ccc"

clean:
	rm -f ccc
