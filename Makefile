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
	@if ! echo "$$PATH" | grep -q "$$HOME/bin"; then \
		if ! grep -q 'export PATH="$$HOME/bin:$$PATH"' ~/.bashrc 2>/dev/null; then \
			echo 'export PATH="$$HOME/bin:$$PATH"' >> ~/.bashrc; \
			echo "✅ Added ~/bin to PATH in ~/.bashrc"; \
			echo "   Run: source ~/.bashrc"; \
		fi \
	fi
	@if systemctl --user is-active ccc >/dev/null 2>&1; then \
		systemctl --user restart ccc; \
		echo "✅ Restarted ccc service"; \
	fi

clean:
	rm -f ccc
