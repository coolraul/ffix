// ffix — find video files that cannot stream to a browser because the moov
// atom is not at the beginning of the file, and report whether
// `ffmpeg -c copy -movflags +faststart` will resolve the issue.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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

// fileResult holds analysis output for a single video file.
type fileResult struct {
	Path         string `json:"path"`
	Format       string `json:"format,omitempty"`
	IsMp4Family  bool   `json:"is_mp4_family"`
	Streamable   bool   `json:"streamable"`
	CanFastStart bool   `json:"can_faststart"`
	FixCommand   string `json:"fix_command,omitempty"`
	Note         string `json:"note,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ── MP4 binary parsing ────────────────────────────────────────────────────────

// parseMoovPosition reads top-level ISO BMFF boxes and returns the position of
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

// ── ffprobe format detection ──────────────────────────────────────────────────

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
}

// probeFormat returns the ffprobe format_name string for the file.
func probeFormat(path string) (string, error) {
	out, err := exec.Command(
		"ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		path,
	).Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe: %w", err)
	}
	var p ffprobeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return "", fmt.Errorf("ffprobe parse: %w", err)
	}
	return p.Format.FormatName, nil
}

// isMp4FormatName returns true when the ffprobe format_name indicates an ISO
// BMFF container (e.g. "mov,mp4,m4a,3gp,3g2,mj2").
func isMp4FormatName(name string) bool {
	mp4Keywords := []string{"mp4", "mov", "m4v", "m4a", "3gp", "3g2", "f4v", "ipod", "mj2"}
	lower := strings.ToLower(name)
	for _, kw := range mp4Keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// ── output helpers ────────────────────────────────────────────────────────────

// faststartCmd builds the ffmpeg fix command for a given file.
func faststartCmd(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	out := base + "_faststart" + ext
	return fmt.Sprintf("ffmpeg -i %q -c copy -movflags +faststart %q", path, out)
}

// convertCmd builds a command to re-mux a non-MP4 file into a streamable MP4.
func convertCmd(path string) string {
	base := strings.TrimSuffix(path, filepath.Ext(path))
	out := base + ".mp4"
	return fmt.Sprintf("ffmpeg -i %q -c:v copy -c:a copy -movflags +faststart %q", path, out)
}

// ── analysis ──────────────────────────────────────────────────────────────────

func analyzeFile(path string, hasFFprobe bool) fileResult {
	ext := strings.ToLower(filepath.Ext(path))
	res := fileResult{Path: path, IsMp4Family: mp4Extensions[ext]}

	// ── Non-MP4 containers ──────────────────────────────────────────────────
	if !res.IsMp4Family {
		res.Streamable = false
		res.CanFastStart = false
		res.Note = "non-MP4 container — no moov atom; faststart does not apply"
		res.FixCommand = convertCmd(path)
		if hasFFprobe {
			if fmt, err := probeFormat(path); err == nil {
				res.Format = fmt
			}
		}
		return res
	}

	// ── MP4-family: check moov position via binary parsing ──────────────────
	status, err := parseMoovPosition(path)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// Optionally enrich with ffprobe format name.
	if hasFFprobe {
		if fmt, err := probeFormat(path); err == nil {
			res.Format = fmt
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

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	var (
		verbose    = flag.Bool("v", false, "show all files, including already-streamable ones")
		includeOther = flag.Bool("all-formats", false, "also report non-MP4 format files")
		jsonOut    = flag.Bool("json", false, "output results as JSON")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `ffix — find video files that will not stream to a browser due to a
late moov atom, and report whether -movflags +faststart will fix them.

Usage:
  ffix [flags] <directory>

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Exit codes:
  0  no issues found (or -v/--all-formats showed everything)
  1  one or more non-streamable files found
  2  usage error / dependency missing
`)
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)

	// Verify the target directory exists.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %q is not an accessible directory\n", dir)
		os.Exit(2)
	}

	// Detect available tools.
	_, hasFFprobe := exec.LookPath("ffprobe")
	hasFF := hasFFprobe == nil
	if !hasFF {
		fmt.Fprintln(os.Stderr, "warning: ffprobe not found — format names will be omitted")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Fprintln(os.Stderr, "warning: ffmpeg not found — fix commands shown but cannot be run here")
	}

	// Walk the directory tree.
	var results []fileResult
	var walkErrs []string

	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walkErrs = append(walkErrs, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		isMp4 := mp4Extensions[ext]
		isOther := otherVideoExtensions[ext]

		if !isMp4 && !isOther {
			return nil
		}
		if isOther && !*includeOther {
			return nil
		}

		r := analyzeFile(path, hasFF)
		results = append(results, r)
		return nil
	})

	sort.Slice(results, func(i, j int) bool {
		return results[i].Path < results[j].Path
	})

	// ── JSON output ─────────────────────────────────────────────────────────
	if *jsonOut {
		var out []fileResult
		for _, r := range results {
			if *verbose || !r.Streamable || r.Error != "" {
				out = append(out, r)
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		for _, e := range walkErrs {
			fmt.Fprintln(os.Stderr, "walk error:", e)
		}
		exitCode(results)
		return
	}

	// ── Text output ──────────────────────────────────────────────────────────
	var shown int
	for _, r := range results {
		if !*verbose && r.Streamable && r.Error == "" {
			continue
		}
		shown++
		fmt.Printf("%s\n", r.Path)
		if r.Format != "" {
			fmt.Printf("  format       : %s\n", r.Format)
		}
		fmt.Printf("  streamable   : %v\n", r.Streamable)
		fmt.Printf("  can-faststart: %v\n", r.CanFastStart)
		if r.Note != "" {
			fmt.Printf("  note         : %s\n", r.Note)
		}
		if r.Error != "" {
			fmt.Printf("  error        : %s\n", r.Error)
		}
		if r.FixCommand != "" {
			fmt.Printf("  fix          : %s\n", r.FixCommand)
		}
		fmt.Println()
	}

	if shown == 0 {
		if *verbose {
			fmt.Println("No video files found.")
		} else {
			fmt.Println("All scanned video files are streamable.")
		}
	}

	for _, e := range walkErrs {
		fmt.Fprintln(os.Stderr, "walk error:", e)
	}

	exitCode(results)
}

// exitCode terminates with code 1 if any non-streamable / errored file exists.
func exitCode(results []fileResult) {
	for _, r := range results {
		if !r.Streamable || r.Error != "" {
			os.Exit(1)
		}
	}
	os.Exit(0)
}
