package index

// docPosting is one document in a term's postings list.
// pos is strictly increasing. Positions count every token in the field,
// including tokens shorter than minTokenLen, so a dropped token leaves a gap.
type docPosting struct {
	doc uint32
	pos []uint32
}

func postingIDs(list []docPosting) []uint32 {
	if len(list) == 0 {
		return nil
	}
	out := make([]uint32, len(list))
	for i, p := range list {
		out[i] = p.doc
	}
	return out
}

func clonePosting(p docPosting, doc uint32) docPosting {
	pos := make([]uint32, len(p.pos))
	copy(pos, p.pos)
	return docPosting{doc: doc, pos: pos}
}
