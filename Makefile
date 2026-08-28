.PHONY: build test install uninstall check doctor deps-ubuntu completion clean help

build:
	@./setup build

test:
	@./setup test

install:
	@./setup install

uninstall:
	@./setup uninstall

check:
	@./setup check

doctor: check

deps-ubuntu:
	@./setup deps-ubuntu

completion:
	@./setup completion

clean:
	@./setup clean

help:
	@./setup help
