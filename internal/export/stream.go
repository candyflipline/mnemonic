package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// writeArray writes doc, a JSON document that holds marker (`"key": []` or
// `"key":[]`) once, with that empty array filled by each, one element at a
// time, laid out as json.Marshal (indent "") or json.MarshalIndent (indent
// "  ") lays out the whole document. each gets the prefix an element's lines
// carry and puts each element's bytes.
func writeArray(w io.Writer, doc []byte, marker, indent string, each func(prefix string, put func([]byte) error) error) error {
	at := bytes.LastIndex(doc, []byte(marker))
	if at < 0 {
		return fmt.Errorf("export: %s is not in the envelope", marker)
	}
	end := at + len(marker) - 1
	keyIndent := ""
	if indent != "" {
		keyIndent = string(doc[bytes.LastIndexByte(doc[:at], '\n')+1 : at])
	}
	prefix := keyIndent + indent
	if _, err := w.Write(doc[:end]); err != nil {
		return err
	}
	n := 0
	err := each(prefix, func(elem []byte) error {
		sep := ","
		if n == 0 {
			sep = ""
		}
		if indent != "" {
			sep += "\n" + prefix
		}
		n++
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
		_, err := w.Write(elem)
		return err
	})
	if err != nil {
		return err
	}
	if n > 0 && indent != "" {
		if _, err := io.WriteString(w, "\n"+keyIndent); err != nil {
			return err
		}
	}
	_, err = w.Write(doc[end:])
	return err
}

func decodeNumbers(b []byte) (any, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	err := dec.Decode(&doc)
	return doc, err
}

func sortedIndent(b []byte, prefix string) ([]byte, error) {
	doc, err := decodeNumbers(b)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, prefix, "  ")
}
