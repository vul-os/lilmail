// handlers/api/mailclient.go — MailClient interface satisfied by both the real
// IMAP *Client and the in-memory *DemoClient used in demo/screenshot mode.
package api

import "lilmail/models"

// MailClient is the abstract mail-backend interface consumed by all web
// handlers. The real implementation (*Client) connects to a live IMAP server;
// *DemoClient returns seeded in-memory data without any network calls.
//
// Methods match the subset of *Client that web handlers actually call.
type MailClient interface {
	// Folder & message listing
	FetchFolders() ([]*MailboxInfo, error)
	FetchMessages(folderName string, limit uint32) ([]models.Email, error)
	FetchSingleMessage(folderName, uid string) (models.Email, error)
	SearchMessages(folderName, query string, limit uint32) ([]models.Email, error)

	// Attachment
	FetchAttachment(folderName, uid, partPath string) ([]byte, string, string, error)

	// Mutating operations
	DeleteMessage(folderName, uid string) error
	SetMessageFlag(folderName, uid string, flag string, add bool) error
	MoveMessage(srcFolder, uid, destFolder string) error
	SaveToSent(to, subject, body string, rawMessage []byte) error
	SaveDraft(rawMessage []byte) error
	DeleteDraft(uid string) error
	DeleteMessageFromFolder(folder, uid string) error
	DiscoverDraftsFolder() (string, error)
	DiscoverTrashFolder() (string, error)

	// Real-time notifications
	WatchInbox(stop <-chan struct{}, onNewMail func(email models.Email)) error

	// Connection management
	Close() error
}

// Compile-time assertions: both implementations must satisfy MailClient.
var _ MailClient = (*Client)(nil)
var _ MailClient = (*DemoClient)(nil)
