// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package vmdef

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot is what vmobj needs from a libvirt snapshot definition
// (virsh snapshot-dumpxml, or the files QNAP Virtualization Station keeps
// in .<uuid>.meta/snapshot/<epoch>.xml).
//
// For an external snapshot, each disk names the overlay file that became
// the disk's new top layer when the snapshot was taken. The snapshot's
// contents are then the layer under that overlay: the file the disk was
// using until that moment.
//
// The definition copy inside (<domain>) is not read: it isn't needed,
// and it can hold a display password.
type Snapshot struct {
	Name string // libvirt's name; QNAP Virtualization Station uses the epoch
	// DisplayName is the name shown in Virtualization Station's UI
	// (<displayname>, a QNAP addition); empty elsewhere.
	DisplayName string
	Description string
	Created     time.Time
	State       string            // shutoff, running, ...
	Overlays    map[string]string // disk target -> overlay file, for disks snapshotted externally
	Internal    []string          // disk targets snapshotted inside their qcow2 file
}

type snapshotXML struct {
	XMLName      xml.Name `xml:"domainsnapshot"`
	Name         string   `xml:"name"`
	DisplayName  string   `xml:"displayname"`
	Description  string   `xml:"description"`
	State        string   `xml:"state"`
	CreationTime string   `xml:"creationTime"`
	Disks        []struct {
		Name     string `xml:"name,attr"`
		Snapshot string `xml:"snapshot,attr"`
		Source   struct {
			File string `xml:"file,attr"`
			Dev  string `xml:"dev,attr"`
		} `xml:"source"`
	} `xml:"disks>disk"`
}

// ParseSnapshot reads a libvirt snapshot definition.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	var x snapshotXML
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	if err := d.Decode(&x); err != nil {
		return nil, fmt.Errorf("snapshot definition: %w", err)
	}
	if strings.TrimSpace(x.Name) == "" {
		return nil, errors.New("snapshot definition has no name")
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(x.CreationTime), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: creationTime %q: %w", x.Name, x.CreationTime, err)
	}
	s := &Snapshot{Name: strings.TrimSpace(x.Name), DisplayName: strings.TrimSpace(x.DisplayName), Description: strings.TrimSpace(x.Description),
		Created: time.Unix(secs, 0).UTC(), State: strings.TrimSpace(x.State), Overlays: map[string]string{}}
	for _, disk := range x.Disks {
		switch disk.Snapshot {
		case "external":
			f := disk.Source.File
			if f == "" {
				f = disk.Source.Dev
			}
			if f == "" {
				return nil, fmt.Errorf("snapshot %s: external disk %s has no overlay file", s.Name, disk.Name)
			}
			s.Overlays[disk.Name] = f
		case "internal":
			s.Internal = append(s.Internal, disk.Name)
		}
	}
	return s, nil
}

// QNAPRecord is a snapshot record to write for QNAP Virtualization
// Station, which keeps one per snapshot at .<uuid>.meta/snapshot/<epoch>.xml
// and lists a VM's snapshots from those files. It is libvirt's snapshot
// XML (named by its epoch, so the name is a number) followed by four
// elements of QNAP's own, in the order and form Virtualization Station
// writes them.
type QNAPRecord struct {
	Epoch       int64             // creation time, UTC; also the record's name and file name
	Parent      int64             // the previous snapshot's epoch; 0 for the first
	DisplayName string            // the name shown in the UI
	Description string            // optional
	Overlays    map[string]string // disk target -> the overlay file this snapshot started
	// LocalOffset is the QNAP's offset from UTC at that moment, in
	// seconds (-28800 for PST): Virtualization Station also stores the
	// local wall-clock time, as if it were UTC.
	LocalOffset int
}

// Marshal writes the record. domain is the VM's definition as it was at
// the snapshot (libvirt keeps a copy in every snapshot); it goes in as is.
func (r QNAPRecord) Marshal(domain []byte) ([]byte, error) {
	if r.Epoch <= 0 || r.DisplayName == "" || len(r.Overlays) == 0 {
		return nil, errors.New("qnap snapshot record: needs an epoch, a display name and at least one disk")
	}
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b bytes.Buffer
	b.WriteString("<domainsnapshot>\n")
	fmt.Fprintf(&b, "  <name>%d</name>\n", r.Epoch)
	if r.Description != "" {
		fmt.Fprintf(&b, "  <description>%s</description>\n", esc(r.Description))
	}
	b.WriteString("  <state>shutoff</state>\n")
	if r.Parent > 0 {
		fmt.Fprintf(&b, "  <parent>\n    <name>%d</name>\n  </parent>\n", r.Parent)
	}
	fmt.Fprintf(&b, "  <creationTime>%d</creationTime>\n", r.Epoch)
	b.WriteString("  <memory snapshot='no'/>\n  <disks>\n")
	targets := make([]string, 0, len(r.Overlays))
	for t := range r.Overlays {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		fmt.Fprintf(&b, "    <disk name='%s' snapshot='external' type='file'>\n      <driver type='qcow2'/>\n      <source file='%s'/>\n    </disk>\n", esc(t), esc(r.Overlays[t]))
	}
	b.WriteString("  </disks>\n")
	if d := bytes.TrimSpace(domain); len(d) > 0 {
		b.Write(d)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "<displayname>%s</displayname><creationLocaltime>%d</creationLocaltime><lockSnapshot>unlock</lockSnapshot><createdBy>user</createdBy></domainsnapshot>\n",
		esc(r.DisplayName), r.Epoch+int64(r.LocalOffset))
	return b.Bytes(), nil
}
