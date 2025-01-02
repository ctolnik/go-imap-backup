// go-imap-backup (C) 2022 by Markus L. Noga
// Backup, restore and delete old messages from an IMAP server
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"log"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	client "github.com/emersion/go-imap/v2/imapclient"

	"math"
	"sort"
	"time"

	pb "github.com/schollz/progressbar/v3"
)

// Retrieves a list of all folders from an Imap server
func ListFolders(c *client.Client) ([]string, error) {
	// Query list of folders
	folders, err := c.List("", "%", nil).Collect()
	if err != nil {
		log.Fatalf("failed to list folders: %v", err)
	}
	log.Printf("Found %v folders", len(folders))
	// Collect results
	mailboxes := []string{}
	for _, folder := range folders {
		log.Printf(" - %v", folder.Mailbox)
		mailboxes = append(mailboxes, folder.Mailbox)
	}

	sort.Strings(mailboxes)
	return mailboxes, nil
}

// Creates local metadata for an imap folder by fetching metadata for all its messages
func NewImapFolderMeta(c *client.Client, folderName string) (ifm *ImapFolderMeta, err error) {
	ifm = &ImapFolderMeta{Name: folderName}
	selectedMbox, err := c.Select(folderName, nil).Wait()
	if err != nil {
		log.Printf("Failed to select %s: %v \n", folderName, err)
		log.Printf("Folder %s doesn't exist \n", folderName)
		return nil, err
	}
	log.Printf("%s contains %v messages", folderName, selectedMbox.NumMessages)
	ifm.UidValidity = selectedMbox.UIDValidity
	if selectedMbox.NumMessages == 0 {
		return ifm, nil
	}

	seqSet := imap.SeqSetNum(1)
	seqSet.AddRange(1, selectedMbox.NumMessages)
	fetchOptions := &imap.FetchOptions{RFC822Size: true, UID: true}
	messages, err := c.Fetch(seqSet, fetchOptions).Collect()
	if err != nil {
		log.Fatalf("failed to fetch messages in %s: %v", folderName, err)
	}
	ifm.Messages = []MessageMeta{}
	for message := range messages {
		msg := messages[message]
		fmt.Println(msg)
		d := MessageMeta{SeqNum: msg.SeqNum, UidValidity: selectedMbox.UIDValidity, Uid: uint32(msg.UID), Size: uint32(msg.RFC822Size), Offset: math.MaxUint64}
		ifm.Messages = append(ifm.Messages, d)
		ifm.Size += uint64(msg.RFC822Size)
	}
	return ifm, nil
}

// Download the given set of messages from the remote Imap mailbox,
// and save them to local folders using the remote folder name,
// reporting download progress in bytes to the progress bar after every message
func (f *ImapFolderMeta) DownloadTo(c *client.Client, lf *LocalFolder, bar *pb.ProgressBar) error {
	// Select mailbox on server
	selectedMbox, err := c.Select(f.Name, nil).Wait()
	if err != nil {
		log.Fatalf("failed to select %s: %v", f.Name, err)
	}
	if selectedMbox.UIDValidity != f.UidValidity {
		return fmt.Errorf("UidValidity changed from %d to %d, this should not happen",
			selectedMbox.UIDValidity, f.UidValidity)
	}
	seqSet := imap.SeqSetNum(1)
	seqSet.AddRange(1, selectedMbox.NumMessages)
	fetchOptions := &imap.FetchOptions{RFC822Size: true, UID: true, Envelope: true, BodySection: []*imap.FetchItemBodySection{{}}}
	fetchCmd := c.Fetch(seqSet, fetchOptions)
	defer fetchCmd.Close()
	for {
		msg := fetchCmd.Next()
		log.Printf("Start Work on items \n")
		if msg == nil {
			break
		}
		var (
			env  string
			uid  uint32
			size int64
			date time.Time
			body []uint8
		)
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			log.Println(item)
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = uint32(item.UID)
			case imapclient.FetchItemDataRFC822Size:
				size = item.Size
			case imapclient.FetchItemDataEnvelope:
				env = item.Envelope.From[0].Addr()
				date = item.Envelope.Date
			case imapclient.FetchItemDataBodySection:
				bs, err := io.ReadAll(item.Literal)
				if err != nil {
					log.Fatalf("failed to read body section: %v", err)
				}
				body = bs
			}
		}
		if body != nil {
			if err := lf.Append(selectedMbox.UIDValidity, uid, env, date, body); err != nil {
				log.Println("Shit happens. Then we try to save data on disk")
				log.Fatal(err)
			}
			// print progress
			if err := bar.Add64(size); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Printf("Body is empty. Skip...")
		}

	}
	if err := fetchCmd.Close(); err != nil {
		log.Fatalf("FETCH command failed: %v", err)
	}

	return nil
}

// Delete messages before the given time from an Imap server
func DeleteMessagesBefore(c *client.Client, folderName string, before time.Time) (numDeleted int) {
	// Select mailbox on server
	selectedMbox, err := c.Select(folderName, nil).Wait()
	if err != nil {
		log.Fatalf("failed to select %s: %v", folderName, err)
	}
	if selectedMbox.NumMessages == 0 {
		return 0
	}
	uidMsg, err := c.UIDSearch(&imap.SearchCriteria{
		Before: before,
	}, nil).Wait()
	if uidMsg.All != nil {
		deleteMessages(c, uidMsg.All)
		return len(uidMsg.AllUIDs())
	}
	return 0
}

func deleteMessages(c *client.Client, ids imap.NumSet) {
	storeFlags := imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagDeleted},
		Silent: true,
	}
	if err := c.Store(ids, &storeFlags, nil).Close(); err != nil {
		log.Fatalf("STORE command failed: %v", err)
	}
	c.Expunge()
}
