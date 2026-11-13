package mail

// A source owns the actual connection. Address/alias rows only select its
// recipient. Temporary HTTP providers belong to common/temp_email.py; they do
// not advertise an IMAP fallback in this ingestion service.
const (
	TransportIMAP  = "imap"
	TransportPOP3  = "pop3"
	TransportGraph = "graph"
)
