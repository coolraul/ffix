package main

import (
	"encoding/binary"
	"io"
	"os"
)

// mp4Extensions are ISO Base Media File Format containers that carry a moov
// atom and support -movflags +faststart.
var mp4Extensions = map[string]bool{
	".mp4": true,
	".m4v": true,
	".m4a": true,
	".mov": true,
	".3gp": true,
	".3g2": true,
	".f4v": true,
}

// otherVideoExtensions are video containers that do NOT use moov atoms.
// They are included so the tool can explain why faststart cannot help.
var otherVideoExtensions = map[string]bool{
	".mkv":  true,
	".webm": true,
	".avi":  true,
	".flv":  true,
	".wmv":  true,
	".ts":   true,
	".mts":  true,
	".m2ts": true,
	".mpeg": true,
	".mpg":  true,
	".ogv":  true,
	".ogg":  true,
	".vob":  true,
	".rm":   true,
	".rmvb": true,
}

// moovStatus describes where the moov atom was found relative to mdat.
type moovStatus int

const (
	moovBeforeMdat moovStatus = iota // file is already streamable
	moovAfterMdat                    // moov is late — faststart will fix this
	moovMissing                      // no moov atom — file may be corrupt/incomplete
	moovNoMdat                       // moov present but no mdat (fragmented / metadata-only)
)

// parseMoovPosition reads top-level ISO BMFF (Base Media File Format) boxes and returns the position of
// the moov atom relative to the mdat atom.  It only reads box headers, never
// the payload, so it stays fast even on multi-gigabyte files.
func parseMoovPosition(path string) (moovStatus, error) {
	f, err := os.Open(path)
	if err != nil {
		return moovMissing, err
	}
	defer f.Close()

	var (
		moovOff = int64(-1)
		mdatOff = int64(-1)
		offset  = int64(0)
	)

	for {
		// Each box starts with 4-byte size + 4-byte FourCC type.
		var hdr [8]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break
		}

		boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		boxType := string(hdr[4:8])
		hdrSize := int64(8)

		switch boxSize {
		case 0:
			// Box extends to end of file — record it and stop.
			switch boxType {
			case "moov":
				moovOff = offset
			case "mdat":
				mdatOff = offset
			}
			goto done
		case 1:
			// 64-bit extended size follows the type field.
			var ext [8]byte
			if _, err := io.ReadFull(f, ext[:]); err != nil {
				goto done
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			hdrSize = 16
		}

		if boxSize < hdrSize {
			break // malformed box
		}

		switch boxType {
		case "moov":
			moovOff = offset
		case "mdat":
			mdatOff = offset
		}

		// Stop as soon as we have seen both atoms — no need to scan further.
		if moovOff >= 0 && mdatOff >= 0 {
			break
		}

		offset += boxSize
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			break
		}
	}

done:
	switch {
	case moovOff < 0:
		return moovMissing, nil
	case mdatOff < 0:
		return moovNoMdat, nil
	case moovOff < mdatOff:
		return moovBeforeMdat, nil
	default:
		return moovAfterMdat, nil
	}
}
