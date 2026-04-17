// ffix — find video files that cannot stream to a browser because the moov
// atom is not at the beginning of the file, and report whether
// `ffmpeg -c copy -movflags +faststart` will resolve the issue.
// For non-MP4 containers it inspects the codec to determine whether a
// lossless re-mux (-c copy) or a full re-encode is needed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ── output helpers ────────────────────────────────────────────────────────────

// faststartCmd builds the ffmpeg fix command for a given file.
func faststartCmd(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	out := base + "_faststart" + ext
	return fmt.Sprintf("ffmpeg -i %q -c copy -movflags +faststart %q", path, out)
}

// remuxOrEncodeCmd builds the ffmpeg command for a non-MP4 file.
// If both codecs are MP4-compatible it uses -c copy (lossless re-mux);
// otherwise it falls back to re-encoding video to H.264 and/or audio to AAC.
func remuxOrEncodeCmd(path, videoCodec, audioCodec string) (cmd, note string) {
	base := strings.TrimSuffix(path, filepath.Ext(path))
	out := base + ".mp4"

	vc, ac := "copy", "copy"
	var reasons []string

	if videoCodec != "" && !canCopyVideo(videoCodec) {
		vc = "libx264"
		reasons = append(reasons, videoCodec+" → H.264 re-encode")
	}
	if audioCodec != "" && !canCopyAudio(audioCodec) {
		ac = "aac"
		reasons = append(reasons, audioCodec+" → AAC re-encode")
	}

	cmd = fmt.Sprintf("ffmpeg -i %q -c:v %s -c:a %s -movflags +faststart %q", path, vc, ac, out)
	if len(reasons) > 0 {
		note = "re-encode required: " + strings.Join(reasons, ", ")
	} else {
		note = "lossless re-mux (codecs are MP4-compatible)"
	}
	return cmd, note
}

// ── fix plan + execution ──────────────────────────────────────────────────────

// fixPlan describes exactly how to fix one file: what ffmpeg will write,
// where it ends up, and whether the original needs a backup first.
type fixPlan struct {
	TmpPath     string // ffmpeg writes here (same filesystem as FinalPath)
	FinalPath   string // TmpPath is renamed here on success
	VideoArg    string // -c:v value
	AudioArg    string // -c:a value
	NeedsBackup bool   // true when FinalPath == original (MP4 in-place fix)
}

// planFix returns a fixPlan for a non-streamable result, or nil when no
// automated fix is available (missing moov, analysis error, etc.).
func planFix(r fileResult) *fixPlan {
	if r.Error != "" || r.Streamable {
		return nil
	}
	if r.IsMp4Family {
		// Only fixable when moov is present but late (CanFastStart == true).
		if !r.CanFastStart {
			return nil
		}
		ext := filepath.Ext(r.Path)
		base := strings.TrimSuffix(r.Path, ext)
		// Preserve the real extension on the tmp file so ffmpeg can detect the
		// muxer without needing an explicit -f flag.
		return &fixPlan{
			TmpPath:     base + ".ffix-tmp" + ext,
			FinalPath:   r.Path,
			VideoArg:    "copy",
			AudioArg:    "copy",
			NeedsBackup: true,
		}
	}
	// Non-MP4: output is a new .mp4 file; original is never touched.
	base := strings.TrimSuffix(r.Path, filepath.Ext(r.Path))
	final := base + ".mp4"
	vc, ac := "copy", "copy"
	if r.VideoCodec != "" && !canCopyVideo(r.VideoCodec) {
		vc = "libx264"
	}
	if r.AudioCodec != "" && !canCopyAudio(r.AudioCodec) {
		ac = "aac"
	}
	return &fixPlan{
		TmpPath:   base + ".ffix-tmp.mp4",
		FinalPath: final,
		VideoArg:  vc,
		AudioArg:  ac,
	}
}

// fixFile executes the fix for r in-place (modifying r's fix fields).
// Safe sequence: ffmpeg → .tmp, verify moov, backup original, rename .tmp.
// The .tmp file is always removed on failure.
func fixFile(r *fileResult, noBackup bool) {
	plan := planFix(*r)
	if plan == nil {
		r.FixError = "no automated fix available for this file"
		return
	}

	// For new-file output, refuse to overwrite an existing file.
	if !plan.NeedsBackup {
		if _, err := os.Stat(plan.FinalPath); err == nil {
			r.FixError = fmt.Sprintf("output already exists: %s", plan.FinalPath)
			return
		}
	}

	// Step 1: run ffmpeg, writing to .tmp
	cmd := exec.Command("ffmpeg",
		"-i", r.Path,
		"-c:v", plan.VideoArg,
		"-c:a", plan.AudioArg,
		"-movflags", "+faststart",
		"-loglevel", "error",
		"-y", // overwrite .tmp if a previous failed run left one
		plan.TmpPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(plan.TmpPath)
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			r.FixError = fmt.Sprintf("ffmpeg: %v: %s", err, msg)
		} else {
			r.FixError = fmt.Sprintf("ffmpeg: %v", err)
		}
		return
	}

	// Step 2: verify moov is now at the start of the output
	status, err := parseMoovPosition(plan.TmpPath)
	if err != nil || status != moovBeforeMdat {
		os.Remove(plan.TmpPath)
		r.FixError = "output verification failed: moov not at start of written file"
		return
	}

	// Step 3: atomic swap
	if plan.NeedsBackup {
		backupPath := r.Path + ".bak"
		if err := os.Rename(r.Path, backupPath); err != nil {
			os.Remove(plan.TmpPath)
			r.FixError = fmt.Sprintf("backup rename failed: %v", err)
			return
		}
		if err := os.Rename(plan.TmpPath, plan.FinalPath); err != nil {
			// Best-effort restore of the original before returning the error.
			os.Rename(backupPath, r.Path)
			os.Remove(plan.TmpPath)
			r.FixError = fmt.Sprintf("swap rename failed: %v", err)
			return
		}
		if noBackup {
			os.Remove(backupPath)
		} else {
			r.BackupPath = backupPath
		}
	} else {
		if err := os.Rename(plan.TmpPath, plan.FinalPath); err != nil {
			os.Remove(plan.TmpPath)
			r.FixError = fmt.Sprintf("rename failed: %v", err)
			return
		}
	}

	r.FixApplied = true
	r.FixOutput = plan.FinalPath
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	var (
		verbose      = flag.Bool("v", false, "show all files, including already-streamable ones")
		includeOther = flag.Bool("all-formats", false, "also report non-MP4 format files")
		jsonOut      = flag.Bool("json", false, "output results as JSON")
		fix          = flag.Bool("fix", false, "fix non-streamable files (write→verify→swap; keeps .bak of originals)")
		noBackup     = flag.Bool("no-backup", false, "with -fix: delete original after successful swap instead of keeping .bak")
		dryRun       = flag.Bool("dry-run", false, "show what -fix would do without making any changes")
		maxFixes     = flag.Int("max-fixes", 0, "with -fix: stop after this many successful fixes (0 = unlimited)")
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
Fix safety:
  -fix writes ffmpeg output to <file>.tmp, verifies the moov atom is at the
  start of the result, then renames the original to <file>.bak before moving
  the .tmp into place.  The .tmp is removed on any failure.
  -no-backup skips the .bak step (original is deleted after a successful swap).
  Non-MP4 files are converted to a new .mp4 alongside the original.

Exit codes:
  0  no issues found (or -v/-all-formats showed everything)
  1  one or more non-streamable files found (or a -fix run had failures)
  2  usage error / dependency missing
`)
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *noBackup && !*fix {
		fmt.Fprintln(os.Stderr, "error: -no-backup requires -fix")
		os.Exit(2)
	}
	if *dryRun && *fix {
		fmt.Fprintln(os.Stderr, "error: -dry-run and -fix are mutually exclusive")
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

	// ── dry-run: show what -fix would do ────────────────────────────────────
	if *dryRun {
		any := false
		for _, r := range results {
			if r.Streamable || r.Error != "" {
				continue
			}
			plan := planFix(r)
			if plan == nil {
				fmt.Printf("no fix available: %s\n", r.Path)
				continue
			}
			any = true
			fmt.Printf("would fix: %s\n", r.Path)
			fmt.Printf("  ffmpeg -i %q -c:v %s -c:a %s -movflags +faststart -loglevel error -y %q\n",
				r.Path, plan.VideoArg, plan.AudioArg, plan.TmpPath)
			fmt.Printf("  verify moov at start of %q\n", plan.TmpPath)
			if plan.NeedsBackup {
				fmt.Printf("  rename %q → %q\n", r.Path, r.Path+".bak")
			}
			fmt.Printf("  rename %q → %q\n", plan.TmpPath, plan.FinalPath)
			fmt.Println()
		}
		if !any {
			fmt.Println("Nothing to fix.")
		}
		exitCode(results)
		return
	}

	// ── fix pass ─────────────────────────────────────────────────────────────
	if *fix {
		if *noBackup {
			fmt.Fprintln(os.Stderr, "warning: -no-backup is set — originals will be deleted after a successful fix")
		}
		var fixed int
		for i := range results {
			if *maxFixes > 0 && fixed >= *maxFixes {
				fmt.Fprintf(os.Stderr, "note: reached -max-fixes %d, stopping\n", *maxFixes)
				break
			}
			r := &results[i]
			if r.Streamable || r.Error != "" {
				continue
			}
			fmt.Printf("fixing: %s\n", r.Path)
			fixFile(r, *noBackup)
			if r.FixError != "" {
				fmt.Printf("  FAILED: %s\n\n", r.FixError)
			} else {
				fixed++
				fmt.Printf("  ok → %s\n", r.FixOutput)
				if r.BackupPath != "" {
					fmt.Printf("  backup → %s\n", r.BackupPath)
				}
				fmt.Println()
			}
		}
	}

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
		if r.VideoCodec != "" {
			fmt.Printf("  video codec  : %s\n", r.VideoCodec)
		}
		if r.AudioCodec != "" {
			fmt.Printf("  audio codec  : %s\n", r.AudioCodec)
		}
		fmt.Printf("  streamable   : %v\n", r.Streamable)
		fmt.Printf("  can-faststart: %v\n", r.CanFastStart)
		if r.Note != "" {
			fmt.Printf("  note         : %s\n", r.Note)
		}
		if r.Error != "" {
			fmt.Printf("  error        : %s\n", r.Error)
		}
		if r.FixCommand != "" && !r.FixApplied {
			fmt.Printf("  fix          : %s\n", r.FixCommand)
		}
		if r.FixApplied {
			fmt.Printf("  fixed        : %s\n", r.FixOutput)
			if r.BackupPath != "" {
				fmt.Printf("  backup       : %s\n", r.BackupPath)
			}
		}
		if r.FixError != "" {
			fmt.Printf("  fix error    : %s\n", r.FixError)
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

// exitCode terminates with 1 if any file is non-streamable, errored, or had a
// fix failure; 0 otherwise.
func exitCode(results []fileResult) {
	for _, r := range results {
		if !r.Streamable || r.Error != "" || r.FixError != "" {
			os.Exit(1)
		}
	}
	os.Exit(0)
}
