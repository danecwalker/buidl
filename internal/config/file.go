package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is a buidl.yaml document edited as a yaml.v3 node tree.
//
// Commands that write the file (environment, add, variable) must go through
// this rather than marshal a struct. A struct round-trip drops the comments
// `buidl init` writes, and those comments are most of a new file's value.
type File struct {
	Path string
	doc  *yaml.Node
}

// Open parses path as a YAML mapping, keeping comments and node style.
func Open(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: root must be a mapping", path)
	}
	return &File{Path: path, doc: &doc}, nil
}

// Save writes the document back with 2-space indent.
func (f *File) Save() error {
	raw, err := f.Bytes()
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, raw, 0o644)
}

// Bytes encodes the document. The leading `---` yaml.v3 emits for a
// DocumentNode is stripped so a file that did not have one does not gain one.
func (f *File) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f.doc); err != nil {
		return nil, fmt.Errorf("encoding %s: %w", f.Path, err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return bytes.TrimPrefix(buf.Bytes(), []byte("---\n")), nil
}

func (f *File) root() *yaml.Node {
	return f.doc.Content[0]
}

// App returns the top-level `app` value.
func (f *File) App() string {
	return f.String("app")
}

// DefaultEnvironment returns `defaultEnvironment`, or "".
func (f *File) DefaultEnvironment() string {
	return f.String("defaultEnvironment")
}

// EnvironmentNames returns the keys of `environments`, in file order.
func (f *File) EnvironmentNames() []string {
	return f.Keys("environments")
}

// AccessoryNames returns the keys of `accessories`, in file order.
func (f *File) AccessoryNames() []string {
	return f.Keys("accessories")
}

// Lookup returns the node at path, or nil.
func (f *File) Lookup(path ...string) *yaml.Node {
	n := f.root()
	for _, key := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		n = mapGet(n, key)
	}
	return n
}

// String returns the scalar at path, or "".
func (f *File) String(path ...string) string {
	n := f.Lookup(path...)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// Keys returns mapping keys at path, or nil.
func (f *File) Keys(path ...string) []string {
	n := f.Lookup(path...)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return mapKeys(n)
}

// Set puts value at path, creating intermediate mappings. A null placeholder
// (`staging:` with an empty body) is promoted to a mapping so overlays can
// grow without replacing the node.
func (f *File) Set(path []string, value *yaml.Node) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	parent, err := f.ensureMapping(path[:len(path)-1]...)
	if err != nil {
		return err
	}
	mapSet(parent, path[len(path)-1], value)
	return nil
}

// SetString sets a plain scalar at path.
func (f *File) SetString(path []string, value string) error {
	return f.Set(path, scalarNode(value))
}

// Delete removes the key at path. Missing keys are a no-op.
func (f *File) Delete(path ...string) bool {
	if len(path) == 0 {
		return false
	}
	parent := f.Lookup(path[:len(path)-1]...)
	if parent == nil {
		parent = f.root()
		if len(path) != 1 {
			return false
		}
	}
	if parent.Kind != yaml.MappingNode {
		return false
	}
	return mapDelete(parent, path[len(path)-1])
}

// AppendUnique adds value to the sequence at path if it is not already there.
func (f *File) AppendUnique(path []string, value string) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	parent, err := f.ensureMapping(path[:len(path)-1]...)
	if err != nil {
		return err
	}
	key := path[len(path)-1]
	seq := mapGet(parent, key)
	if seq == nil || isNull(seq) {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		mapSet(parent, key, seq)
	}
	if seq.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s is not a sequence", strings.Join(path, "."))
	}
	for _, c := range seq.Content {
		if c.Value == value {
			return nil
		}
	}
	seq.Style = 0
	seq.Content = append(seq.Content, scalarNode(value))
	return nil
}

// RemoveFromSequence deletes value from the sequence at path.
func (f *File) RemoveFromSequence(path []string, value string) error {
	seq := f.Lookup(path...)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	out := seq.Content[:0]
	for _, c := range seq.Content {
		if c.Value != value {
			out = append(out, c)
		}
	}
	seq.Content = out
	return nil
}

func (f *File) ensureMapping(path ...string) (*yaml.Node, error) {
	n := f.root()
	for i, key := range path {
		child := mapGet(n, key)
		if child == nil || isNull(child) {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mapSet(n, key, child)
		}
		if child.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a mapping", strings.Join(path[:i+1], "."))
		}
		n = child
	}
	return n, nil
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "" || n.Value == "null" || n.Value == "~")
}

func scalarNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func mapIndex(m *yaml.Node, key string) int {
	for i := 0; i < len(m.Content)-1; i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if i := mapIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

func mapSet(m *yaml.Node, key string, value *yaml.Node) {
	if i := mapIndex(m, key); i >= 0 {
		m.Content[i+1] = value
		return
	}
	m.Content = append(m.Content, scalarNode(key), value)
}

func mapDelete(m *yaml.Node, key string) bool {
	i := mapIndex(m, key)
	if i < 0 {
		return false
	}
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	return true
}

func mapKeys(m *yaml.Node) []string {
	var keys []string
	for i := 0; i < len(m.Content)-1; i += 2 {
		keys = append(keys, m.Content[i].Value)
	}
	return keys
}

// CloneNode deep-copies n so a duplicated environment overlay is independent
// of the source node.
func CloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Alias != nil {
		c.Alias = CloneNode(n.Alias)
	}
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, ch := range n.Content {
			c.Content[i] = CloneNode(ch)
		}
	}
	return &c
}

// ParseNode decodes YAML into a mapping or sequence node.
func ParseNode(raw string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &doc, nil
}
