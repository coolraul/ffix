package main

import (
	"path/filepath"
	"strings"
)

// fileResult holds analysis output for a single video file.
type fileResult struct {
	Path         string `json:"path"`
	Format       string `json:"format,omitempty"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	IsMp4Family  bool   `json:"is_mp4_family"`
	Streamable   bool   `json:"streamable"`
	CanFastStart bool   `json:"can_faststart"`
	FixCommand   string `json:"fix_command,omitempty"`
	Note         string `json:"note,omitempty"`
	Error        string `json:"error,omitempty"`
	// populated when -fix or -dry-run is used
	FixApplied bool   `json:"fix_applied,omitempty"`
	FixOutput  string `json:"fix_output,omitempty"`  // path of the fixed/output file
	BackupPath string `json:"backup_path,omitempty"` // original renamed here (.bak)
	FixError   string `json:"fix_error,omitempty"`
}

func analyzeFile(path string, hasFFprobe bool) fileResult {
	ext := strings.ToLower(filepath.Ext(path))
	res := fileResult{Path: path, IsMp4Family: mp4Extensions[ext]}

	// ── Non-MP4 containers ──────────────────────────────────────────────────
	if !res.IsMp4Family {
		res.Streamable = false
		res.CanFastStart = false
		if hasFFprobe {
			fmtName, vc, ac, err := probeFile(path)
			if err == nil {
				res.Format = fmtName
				res.VideoCodec = vc
				res.AudioCodec = ac
				cmd, note := remuxOrEncodeCmd(path, vc, ac)
				res.FixCommand = cmd
				res.Note = "non-MP4 container — no moov atom; faststart does not apply; " + note
			} else {
				res.Note = "non-MP4 container — no moov atom; faststart does not apply"
				res.Error = err.Error()
			}
		} else {
			res.Note = "non-MP4 container — no moov atom; faststart does not apply"
		}
		return res
	}

	// ── MP4-family: check moov position via binary parsing ──────────────────
	status, err := parseMoovPosition(path)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// Optionally enrich with ffprobe format name and codecs.
	if hasFFprobe {
		if fmtName, vc, ac, err := probeFile(path); err == nil {
			res.Format = fmtName
			res.VideoCodec = vc
			res.AudioCodec = ac
		}
	}

	switch status {
	case moovBeforeMdat:
		res.Streamable = true
		res.CanFastStart = true
		res.Note = "moov atom is at start — already streamable"

	case moovAfterMdat:
		res.Streamable = false
		res.CanFastStart = true
		res.Note = "moov atom is after mdat — not browser-streamable"
		res.FixCommand = faststartCmd(path)

	case moovMissing:
		res.Streamable = false
		res.CanFastStart = false
		res.Note = "moov atom not found — file may be incomplete or corrupt"

	case moovNoMdat:
		// moov present, no mdat: fragmented MP4 or metadata-only — typically fine.
		res.Streamable = true
		res.CanFastStart = true
		res.Note = "moov atom present, no mdat (fragmented/metadata-only) — likely streamable"
	}

	return res
}
