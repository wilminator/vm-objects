// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package vmdef reads the parts of a libvirt domain definition (the XML
// that "virsh dumpxml" prints) that a backup needs, and rewrites disk and
// NVRAM paths for a restore.
//
// Rewriting edits only the attribute values that change, in place in the
// original text, so everything else in the definition (comments, order,
// hypervisor-specific elements and namespaces) comes back exactly as it was.
package vmdef

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Disk is one disk of a domain.
type Disk struct {
	Target   string // guest device name: vda, sda, ...
	Type     string // "file" or "block" (others are refused)
	Format   string // driver type: qcow2, raw
	Path     string // source file or device
	ReadOnly bool
	Shared   bool
}

// Domain is what a backup needs from a definition.
type Domain struct {
	Name  string
	UUID  string
	Disks []Disk
	// NVRAM is the UEFI variable store, if the domain has one.
	NVRAM string
	// TPM is true when the domain has an emulated (swtpm) TPM, whose state
	// lives outside the definition, in a directory named by UUID.
	TPM        bool
	TPMVersion string
	// Skipped lists devices that are not backed up, with the reason.
	Skipped []string
}

// Parse reads a domain definition.
func Parse(data []byte) (*Domain, error) { return parse(data, true) }

func parse(data []byte, needUUID bool) (*Domain, error) {
	var raw struct {
		Name string `xml:"name"`
		UUID string `xml:"uuid"`
		OS   struct {
			NVRAM string `xml:"nvram"`
		} `xml:"os"`
		Devices struct {
			Disks []struct {
				Type   string `xml:"type,attr"`
				Device string `xml:"device,attr"`
				Driver struct {
					Type string `xml:"type,attr"`
				} `xml:"driver"`
				Source struct {
					File string `xml:"file,attr"`
					Dev  string `xml:"dev,attr"`
				} `xml:"source"`
				Target struct {
					Dev string `xml:"dev,attr"`
				} `xml:"target"`
				ReadOnly  *struct{} `xml:"readonly"`
				Shareable *struct{} `xml:"shareable"`
			} `xml:"disk"`
			TPMs []struct {
				Backend struct {
					Type    string `xml:"type,attr"`
					Version string `xml:"version,attr"`
				} `xml:"backend"`
			} `xml:"tpm"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("reading the domain definition: %w", err)
	}
	d := &Domain{Name: raw.Name, UUID: strings.ToLower(raw.UUID), NVRAM: strings.TrimSpace(raw.OS.NVRAM)}
	if d.Name == "" || (needUUID && d.UUID == "") {
		return nil, errors.New("the definition has no name or UUID")
	}
	seen := map[string]bool{}
	for _, x := range raw.Devices.Disks {
		if x.Device != "" && x.Device != "disk" {
			d.Skipped = append(d.Skipped, fmt.Sprintf("%s %s (%s)", x.Device, x.Target.Dev, firstNonEmpty(x.Source.File, x.Source.Dev, "empty")))
			continue
		}
		disk := Disk{Target: x.Target.Dev, Type: x.Type, Format: x.Driver.Type,
			ReadOnly: x.ReadOnly != nil, Shared: x.Shareable != nil}
		switch x.Type {
		case "file":
			disk.Path = x.Source.File
		case "block":
			disk.Path = x.Source.Dev
		default:
			return nil, fmt.Errorf("disk %s is of type %q; only file and block disks are supported", x.Target.Dev, x.Type)
		}
		if disk.Target == "" || disk.Path == "" {
			return nil, fmt.Errorf("a disk has no target or no source")
		}
		if seen[disk.Target] {
			return nil, fmt.Errorf("two disks use target %s", disk.Target)
		}
		seen[disk.Target] = true
		if disk.Format == "" {
			disk.Format = "raw"
		}
		d.Disks = append(d.Disks, disk)
	}
	sort.Slice(d.Disks, func(i, j int) bool { return d.Disks[i].Target < d.Disks[j].Target })
	for _, t := range raw.Devices.TPMs {
		switch t.Backend.Type {
		case "emulator":
			d.TPM, d.TPMVersion = true, firstNonEmpty(t.Backend.Version, "1.2")
		case "passthrough":
			d.Skipped = append(d.Skipped, "TPM passthrough (state lives in the host's TPM chip and can't be backed up)")
		}
	}
	return d, nil
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// Changes says what to rewrite in a definition.
type Changes struct {
	// Disks maps a target (vda) to its new source path and format.
	Disks map[string]DiskChange
	// NVRAM, when set, is the new UEFI variable store path.
	NVRAM string
	// Name, when set, renames the domain.
	Name string
	// DropUUID removes the <uuid> element, so libvirt assigns a new one
	// and the copy can be defined next to the original.
	DropUUID bool
}

// DiskChange is a disk's new location.
type DiskChange struct {
	Path   string
	Format string // "raw" or "qcow2"; empty keeps the old one
	// AsFile turns a block disk into a file disk (a zvol restored to an
	// image file).
	AsFile bool
}

// edit replaces data[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// Rewrite returns the definition with disk sources, disk formats and the
// NVRAM path changed. Every disk in ch.Disks must be found.
func Rewrite(data []byte, ch Changes) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		edits   []edit
		stack   []string
		inDisk  bool
		diskTag tagSpan // the <disk> start tag
		driver  tagSpan
		source  tagSpan
		target  string
		found   = map[string]bool{}
		nvram   bool
		inName  bool
		uuidAt  = -1
	)
	prev := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the domain definition: %w", err)
		}
		end := int(dec.InputOffset())
		span := tagSpan{prev, end}
		prev = end
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			path := strings.Join(stack, "/")
			switch {
			case path == "domain/devices/disk":
				inDisk, diskTag, driver, source, target = attr(t, "device", "disk") == "disk", span, tagSpan{}, tagSpan{}, ""
			case inDisk && path == "domain/devices/disk/driver":
				driver = span
			case inDisk && path == "domain/devices/disk/source":
				source = span
			case inDisk && path == "domain/devices/disk/target":
				target = attr(t, "dev", "")
			case path == "domain/os/nvram":
				nvram = true
			case path == "domain/name":
				inName = true
			case path == "domain/uuid":
				uuidAt = span.start
			}
		case xml.CharData:
			if nvram && ch.NVRAM != "" && len(bytes.TrimSpace(t)) > 0 {
				edits = append(edits, edit{span.start, span.end, escape(ch.NVRAM)})
			}
			if inName && ch.Name != "" && len(bytes.TrimSpace(t)) > 0 {
				edits = append(edits, edit{span.start, span.end, escape(ch.Name)})
			}
		case xml.EndElement:
			path := strings.Join(stack, "/")
			switch path {
			case "domain/os/nvram":
				nvram = false
			case "domain/name":
				inName = false
			case "domain/uuid":
				if ch.DropUUID && uuidAt >= 0 {
					edits = append(edits, edit{uuidAt, span.end, ""})
				}
			}
			if path == "domain/devices/disk" && inDisk {
				if c, ok := ch.Disks[target]; ok {
					es, err := diskEdits(data, diskTag, driver, source, c)
					if err != nil {
						return nil, fmt.Errorf("disk %s: %w", target, err)
					}
					edits = append(edits, es...)
					found[target] = true
				}
				inDisk = false
			}
			stack = stack[:len(stack)-1]
		}
	}
	for t := range ch.Disks {
		if !found[t] {
			return nil, fmt.Errorf("disk %s is not in the definition", t)
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), data...)
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	// The result must still parse.
	if _, err := parse(out, !ch.DropUUID); err != nil {
		return nil, fmt.Errorf("rewritten definition does not parse: %w", err)
	}
	return out, nil
}

type tagSpan struct{ start, end int }

func (s tagSpan) ok() bool { return s.end > s.start }

func diskEdits(data []byte, disk, driver, source tagSpan, c DiskChange) ([]edit, error) {
	if !source.ok() {
		return nil, errors.New("has no <source> element")
	}
	var edits []edit
	// The source path is on file= for file disks and dev= for block disks.
	name := "file"
	if _, _, err := findAttr(data[source.start:source.end], "file"); err != nil {
		name = "dev"
	}
	e, err := setAttr(data, source, name, c.Path)
	if err != nil {
		return nil, err
	}
	edits = append(edits, e)
	if c.AsFile && name == "dev" {
		// A block disk restored to an image file: <disk type='file'> and
		// <source file='...'>.
		e, err := setAttr(data, disk, "type", "file")
		if err != nil {
			return nil, err
		}
		r, err := renameAttr(data, source, "dev", "file")
		if err != nil {
			return nil, err
		}
		edits = append(edits, e, r)
	}
	if c.Format != "" {
		if !driver.ok() {
			return nil, errors.New("has no <driver> element to set the format on")
		}
		e, err := setAttr(data, driver, "type", c.Format)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	return edits, nil
}

// setAttr returns an edit replacing one attribute's value inside a tag.
func setAttr(data []byte, tag tagSpan, name, value string) (edit, error) {
	vs, ve, err := findAttr(data[tag.start:tag.end], name)
	if err != nil {
		return edit{}, err
	}
	return edit{tag.start + vs, tag.start + ve, escape(value)}, nil
}

// renameAttr returns an edit renaming an attribute (keeping its value).
func renameAttr(data []byte, tag tagSpan, from, to string) (edit, error) {
	text := data[tag.start:tag.end]
	vs, _, err := findAttr(text, from)
	if err != nil {
		return edit{}, err
	}
	// The name ends just before '=' (with optional spaces) preceding the quote.
	i := vs - 1 // the quote
	for i > 0 && text[i-1] != '=' {
		i--
	}
	j := i - 1 // '='
	for j > 0 && (text[j-1] == ' ' || text[j-1] == '\t' || text[j-1] == '\n' || text[j-1] == '\r') {
		j--
	}
	return edit{tag.start + j - len(from), tag.start + j, to}, nil
}

// findAttr locates the value of attribute name in a start tag's text and
// returns the value's start and end offsets (inside the quotes).
func findAttr(tag []byte, name string) (int, int, error) {
	i := 0
	for i < len(tag) {
		// Find the next occurrence of name preceded by whitespace.
		k := bytes.Index(tag[i:], []byte(name))
		if k < 0 {
			break
		}
		k += i
		i = k + len(name)
		if k == 0 || !isSpace(tag[k-1]) {
			continue
		}
		j := i
		for j < len(tag) && isSpace(tag[j]) {
			j++
		}
		if j >= len(tag) || tag[j] != '=' {
			continue
		}
		j++
		for j < len(tag) && isSpace(tag[j]) {
			j++
		}
		if j >= len(tag) || (tag[j] != '\'' && tag[j] != '"') {
			continue
		}
		q := tag[j]
		end := bytes.IndexByte(tag[j+1:], q)
		if end < 0 {
			return 0, 0, fmt.Errorf("unterminated %s attribute", name)
		}
		return j + 1, j + 1 + end, nil
	}
	return 0, 0, fmt.Errorf("no %s attribute", name)
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func attr(t xml.StartElement, name, def string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return def
}

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(strings.ReplaceAll(b.String(), "'", "&apos;"), `"`, "&quot;")
}
