package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"codeberg.org/pluja/whishper/models"
)

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
}

type ffprobeStream struct {
	Index     int               `json:"index"`
	CodecName string            `json:"codec_name"`
	Tags      map[string]string `json:"tags"`
}

func ExtractSubtitleTracks(mediaPath string) ([]models.SubtitleTrack, error) {
	streams, err := probeSubtitleStreams(mediaPath)
	if err != nil {
		return nil, err
	}

	tracks := make([]models.SubtitleTrack, 0, len(streams))
	for i, stream := range streams {
		result, err := extractSubtitleStream(mediaPath, i, languageFromTags(stream.Tags))
		if err != nil {
			log.Debug().Err(err).Msgf("Error extracting subtitle stream %v", stream.Index)
			continue
		}

		track := models.SubtitleTrack{
			ID:       fmt.Sprintf("%d", stream.Index),
			Index:    stream.Index,
			Language: result.Language,
			Title:    stream.Tags["title"],
			Codec:    stream.CodecName,
			Result:   result,
		}
		tracks = append(tracks, track)
	}

	return tracks, nil
}

func probeSubtitleStreams(mediaPath string) ([]ffprobeStream, error) {
	cmd := exec.CommandContext(context.Background(), "ffprobe", "-v", "error", "-select_streams", "s", "-show_entries", "stream=index,codec_name:stream_tags=language,title", "-of", "json", mediaPath)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var probe ffprobeOutput
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, err
	}

	return probe.Streams, nil
}

func extractSubtitleStream(mediaPath string, subtitleIndex int, language string) (models.WhisperResult, error) {
	tmpDir, err := os.MkdirTemp("", "whishper-subtitle-*")
	if err != nil {
		return models.WhisperResult{}, err
	}
	defer os.RemoveAll(tmpDir)

	outPath := filepath.Join(tmpDir, "subtitle.srt")
	cmd := exec.CommandContext(context.Background(), "ffmpeg", "-y", "-i", mediaPath, "-map", fmt.Sprintf("0:s:%d", subtitleIndex), outPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return models.WhisperResult{}, fmt.Errorf("ffmpeg subtitle extraction failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}

	content, err := os.ReadFile(outPath)
	if err != nil {
		return models.WhisperResult{}, err
	}

	return ParseSRT(string(content), language), nil
}

func ParseSRT(content string, language string) models.WhisperResult {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	blocks := regexp.MustCompile(`\n{2,}`).Split(strings.TrimSpace(content), -1)
	segments := make([]models.Segment, 0, len(blocks))
	texts := make([]string, 0, len(blocks))
	duration := 0.0

	for _, block := range blocks {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}

		timeLineIndex := 0
		if !strings.Contains(lines[timeLineIndex], "-->") && len(lines) > 1 {
			timeLineIndex = 1
		}
		if !strings.Contains(lines[timeLineIndex], "-->") || len(lines) <= timeLineIndex+1 {
			continue
		}

		parts := strings.Split(lines[timeLineIndex], "-->")
		if len(parts) != 2 {
			continue
		}

		start, err := parseSubtitleTime(parts[0])
		if err != nil {
			continue
		}
		end, err := parseSubtitleTime(parts[1])
		if err != nil {
			continue
		}

		text := cleanSubtitleText(strings.Join(lines[timeLineIndex+1:], "\n"))
		if text == "" {
			continue
		}

		segments = append(segments, models.Segment{
			ID:    fmt.Sprintf("%d", len(segments)),
			Start: start,
			End:   end,
			Text:  text,
			Words: []models.Word{},
		})
		texts = append(texts, text)
		if end > duration {
			duration = end
		}
	}

	if language == "" || language == "und" {
		language = "auto"
	}

	return models.WhisperResult{
		Language: language,
		Duration: duration,
		Segments: segments,
		Text:     strings.Join(texts, "\n"),
	}
}

func parseSubtitleTime(value string) (float64, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return 0, fmt.Errorf("invalid subtitle timestamp %q", value)
	}
	value = fields[0]
	value = strings.Replace(value, ",", ".", 1)
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("invalid subtitle timestamp %q", value)
	}

	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return 0, err
	}

	return float64(hours*3600+minutes*60) + seconds, nil
}

func cleanSubtitleText(text string) string {
	text = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(text, "\n")
	text = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	lines := strings.Split(text, "\n")
	cleanLines := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleanLines = append(cleanLines, line)
		}
	}
	return strings.Join(cleanLines, "\n")
}

func languageFromTags(tags map[string]string) string {
	if tags == nil || tags["language"] == "" || tags["language"] == "und" {
		return "auto"
	}
	switch language := strings.ToLower(tags["language"]); language {
	case "eng":
		return "en"
	case "spa":
		return "es"
	case "fre", "fra":
		return "fr"
	case "ger", "deu":
		return "de"
	case "chi", "zho":
		return "zh"
	case "ita":
		return "it"
	case "por":
		return "pt"
	case "rus":
		return "ru"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	case "ara":
		return "ar"
	case "hin":
		return "hi"
	case "dut", "nld":
		return "nl"
	case "pol":
		return "pl"
	case "tur":
		return "tr"
	case "ukr":
		return "uk"
	case "vie":
		return "vi"
	case "ind":
		return "id"
	case "tha":
		return "th"
	case "cze", "ces":
		return "cs"
	case "swe":
		return "sv"
	case "dan":
		return "da"
	case "fin":
		return "fi"
	case "gre", "ell":
		return "el"
	case "heb":
		return "he"
	default:
		return language
	}
}

func FindSubtitleTrack(tracks []models.SubtitleTrack, trackID string) (int, bool) {
	for i, track := range tracks {
		if track.ID == trackID {
			return i, true
		}
	}
	return 0, false
}

func SortSubtitleTracks(tracks []models.SubtitleTrack) {
	sort.Slice(tracks, func(i int, j int) bool {
		return tracks[i].Index < tracks[j].Index
	})
}
