package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"` // "video" | "audio" | "subtitle" | …
		CodecName string `json:"codec_name"`
	} `json:"streams"`
}

// probeFile returns the ffprobe format_name and the first video/audio codec
// names found in the file.
func probeFile(path string) (formatName, videoCodec, audioCodec string, err error) {
	out, err := exec.Command(
		"ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	).Output()
	if err != nil {
		return "", "", "", fmt.Errorf("ffprobe: %w", err)
	}
	var p ffprobeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return "", "", "", fmt.Errorf("ffprobe parse: %w", err)
	}
	formatName = p.Format.FormatName
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if videoCodec == "" {
				videoCodec = s.CodecName
			}
		case "audio":
			if audioCodec == "" {
				audioCodec = s.CodecName
			}
		}
	}
	return formatName, videoCodec, audioCodec, nil
}

// mp4CompatibleVideo lists video codecs that can be placed inside an MP4
// container with -c:v copy (no re-encode needed).
var mp4CompatibleVideo = map[string]bool{
	"h264":       true, // universally supported in browsers
	"hevc":       true, // Safari + hardware decoders; valid in MP4
	"av1":        true, // modern browsers via ISO BMFF
	"mpeg4":      true, // older MPEG-4 Part 2; valid in MP4
	"mjpeg":      true,
	"mpeg1video": false,
	"mpeg2video": false,
	"vp8":        false, // WebM only
	"vp9":        false, // re-mux to MP4 technically possible but browser support inconsistent
	"theora":     false,
	"wmv1":       false,
	"wmv2":       false,
	"wmv3":       false,
	"vc1":        false,
}

// mp4CompatibleAudio lists audio codecs that can be placed inside an MP4
// container with -c:a copy.
var mp4CompatibleAudio = map[string]bool{
	"aac":    true,
	"mp3":    true,
	"ac3":    true,
	"eac3":   true,
	"alac":   true,
	"mp2":    true,
	"vorbis": false, // Ogg/WebM only
	"opus":   false, // RFC 7845 allows MP4 but browser support is inconsistent
	"wmav1":  false,
	"wmav2":  false,
	"flac":   false, // valid in MP4 but not broadly supported in browsers
}

func canCopyVideo(codec string) bool { return mp4CompatibleVideo[strings.ToLower(codec)] }
func canCopyAudio(codec string) bool { return mp4CompatibleAudio[strings.ToLower(codec)] }
