include $(GOROOT)/src/Make.inc

TARG=imap
GOFILES=\
	imap.go\
	parser.go\
	main.go\

include $(GOROOT)/src/Make.cmd
