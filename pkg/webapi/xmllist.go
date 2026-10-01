package webapi

import (
	"encoding/xml"
	"io"
	"strings"
)

// Item is one element of a nested list result (an <eventLogEntry>, a
// <sensor>, ...): its child elements flattened to text, with attributes
// merged in.
type Item map[string]string

type xnode struct {
	name     string
	attrs    map[string]string
	text     strings.Builder
	children []*xnode
}

func (n *xnode) hasElementChildren() bool { return len(n.children) > 0 }

// ParseList parses the inner XML of a list value (the content of
// <temperatures>, <eventLogEntries>, ...) into its items. The iDRAC6 wraps
// some lists in an extra container (<temperatures><thresholdSensorList>
// <sensor>...), so single-child wrapper levels are skipped until the level
// whose children are the records (elements with leaf children).
func ParseList(inner string) []Item {
	root := parseTree("<l>" + inner + "</l>")
	if root == nil {
		return nil
	}
	node := root
	for len(node.children) == 1 && node.children[0].hasElementChildren() {
		child := node.children[0]
		leafOnly := true
		for _, gc := range child.children {
			if gc.hasElementChildren() {
				leafOnly = false
				break
			}
		}
		if leafOnly {
			break // child is itself a record
		}
		node = child
	}
	items := make([]Item, 0, len(node.children))
	for _, rec := range node.children {
		it := Item{}
		for k, v := range rec.attrs {
			it[k] = v
		}
		for _, f := range rec.children {
			it[f.name] = strings.TrimSpace(f.text.String())
		}
		if len(rec.children) == 0 {
			it["value"] = strings.TrimSpace(rec.text.String())
		}
		items = append(items, it)
	}
	return items
}

func parseTree(s string) *xnode {
	dec := xml.NewDecoder(strings.NewReader(s))
	dec.Strict = false
	var stack []*xnode
	var root *xnode
	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{name: t.Name.Local, attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.attrs[a.Name.Local] = a.Value
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			} else {
				root = n
			}
			stack = append(stack, n)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return root
}
