package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// deleted.bits is the only mutable file inside a segment. Postings stay immutable.
//
//	magic "MXDL" | version uint32 | bitCount uint32 | packed bits
//
// Bit i (LSB of byte 0 is doc 0) marks local docID i deleted. Search skips it.
// Merge drops it and does not copy the bit. A missing file means nothing is deleted.

const deletedMagic = "MXDL"

// DeleteResult counts live copies marked deleted. RAM and each segment
// that still stores the _id each contribute one.
type DeleteResult struct {
	ID      string `json:"_id"`
	Deleted int    `json:"deleted"`
}

// Delete marks every live copy of id. Segment copies get a bit in deleted.bits.
// The RAM copy is dropped immediately. Merge is what rewrites postings without them.
func (idx *Index) Delete(id string) (DeleteResult, error) {
	if idx == nil || idx.live == nil || idx.ram == nil {
		return DeleteResult{}, fmt.Errorf("index is not open")
	}
	if id == "" {
		return DeleteResult{}, fmt.Errorf("%w: empty _id", ErrDocNotFound)
	}

	idx.live.mu.Lock()
	defer idx.live.mu.Unlock()

	deleted := 0
	if idx.ram.deleteID(id) {
		deleted++
	}

	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		return DeleteResult{}, err
	}
	for _, ref := range cp.Segments {
		n, err := markSegmentDeleted(filepath.Join(idx.SegmentsDir(), ref.ID), ref.ID, id)
		if err != nil {
			return DeleteResult{}, err
		}
		deleted += n
	}
	if deleted == 0 {
		return DeleteResult{}, fmt.Errorf("%w: %s", ErrDocNotFound, id)
	}
	return DeleteResult{ID: id, Deleted: deleted}, nil
}

func markSegmentDeleted(dir, segID, id string) (int, error) {
	seg, err := openSegment(dir, segID)
	if err != nil {
		return 0, err
	}
	var hits []uint32
	for docID := range seg.docOff {
		if seg.isDeleted(uint32(docID)) {
			continue
		}
		fields, err := seg.fetch(uint32(docID))
		if err != nil {
			return 0, err
		}
		if fields["_id"] == id {
			hits = append(hits, uint32(docID))
		}
	}
	if len(hits) == 0 {
		return 0, nil
	}
	nDocs := len(seg.docOff)
	bits := make([]byte, (nDocs+7)/8)
	copy(bits, seg.deleted)
	for _, docID := range hits {
		bits[docID/8] |= 1 << uint(docID%8)
	}
	if err := writeDeletedBits(filepath.Join(dir, "deleted.bits"), nDocs, bits); err != nil {
		return 0, err
	}
	return len(hits), nil
}

func readDeletedBits(path string, nDocs int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("deleted.bits: %w", err)
	}
	return decodeDeletedBits(data, nDocs)
}

func decodeDeletedBits(data []byte, nDocs int) ([]byte, error) {
	if err := checkHeader(data, deletedMagic); err != nil {
		return nil, fmt.Errorf("deleted.bits: %w", err)
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("deleted.bits: short header")
	}
	bitCount := binary.LittleEndian.Uint32(data[8:12])
	if int(bitCount) != nDocs {
		return nil, fmt.Errorf("deleted.bits: bit count %d, docs %d", bitCount, nDocs)
	}
	want := (nDocs + 7) / 8
	bits := data[12:]
	if len(bits) != want {
		return nil, fmt.Errorf("deleted.bits: %d bytes, want %d", len(bits), want)
	}
	return bits, nil
}

func writeDeletedBits(path string, nDocs int, bits []byte) error {
	want := (nDocs + 7) / 8
	if len(bits) != want {
		return fmt.Errorf("deleted.bits: %d bytes, want %d", len(bits), want)
	}
	buf := make([]byte, 0, 12+len(bits))
	buf = append(buf, deletedMagic...)
	buf = appendU32(buf, formatVersion)
	buf = appendU32(buf, uint32(nDocs))
	buf = append(buf, bits...)
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, buf); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace deleted.bits: %w", err)
	}
	return syncDir(filepath.Dir(path))
}
