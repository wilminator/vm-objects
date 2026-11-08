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
	Name        string
	Description string
	Created     time.Time
	State       string            // shutoff, running, ...
	Overlays    map[string]string // disk target -> overlay file, for disks snapshotted externally
	Internal    []string          // disk targets snapshotted inside their qcow2 file
}

type snapshotXML struct {
	XMLName      xml.Name `xml:"domainsnapshot"`
	Name         string   `xml:"name"`
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
	s := &Snapshot{Name: strings.TrimSpace(x.Name), Description: strings.TrimSpace(x.Description),
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
