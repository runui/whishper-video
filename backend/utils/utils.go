package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"codeberg.org/pluja/whishper/models"
)

func SanitizeFilename(filename string) string {
	// First remove trailing spaces
	filename = strings.TrimSpace(filename)
	// Then remove quotes and dots
	filename = strings.Trim(filename, `"'.`)
	reg, _ := regexp.Compile("[^a-zA-Z0-9._-]+")
	filename = reg.ReplaceAllString(filename, "_")
	filename = strings.Trim(filename, "_")
	if filename == "" {
		filename = "media"
	}
	return filename
}

func DownloadMedia(t *models.Transcription) (string, error) {
	if t.SourceUrl == "" {
		log.Debug().Msg("Source URL is empty")
		return "", fmt.Errorf("source URL is empty")
	}

	if t.ID == primitive.NilObjectID {
		log.Debug().Msg("Transcription ID is empty")
		return "", fmt.Errorf("transcription ID is empty")
	}

	uploadDir := os.Getenv("UPLOAD_DIR")
	filename := fmt.Sprintf("%v%v%%(title).200B.%%(ext)s", t.ID.Hex(), models.FileNameSeparator)
	outputTemplate := filepath.Join(uploadDir, filename)
	cmd := exec.CommandContext(context.Background(), "yt-dlp", "--no-playlist", "--restrict-filenames", "-f", "bestvideo+bestaudio/best", "--merge-output-format", "mp4", "-o", outputTemplate, "--print", "after_move:filepath", t.SourceUrl)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Debug().Err(err).Msgf("yt-dlp failed: %s", strings.TrimSpace(stderr.String()))
		return downloadDirectMedia(t.SourceUrl, uploadDir, t.ID.Hex())
	}

	filePath := lastNonEmptyLine(string(output))
	if filePath == "" {
		return "", fmt.Errorf("yt-dlp did not return a downloaded file path")
	}

	return filepath.Base(filePath), nil

}

func downloadDirectMedia(sourceURL string, uploadDir string, id string) (string, error) {
	resp, err := http.Get(sourceURL)
	if err != nil {
		log.Debug().Err(err).Msg("Error downloading direct media URL")
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("direct media download returned status %v", resp.StatusCode)
	}

	filename := directMediaFilename(sourceURL, resp.Header.Get("Content-Disposition"), resp.Header.Get("Content-Type"))
	filename = fmt.Sprintf("%v%v%v", id, models.FileNameSeparator, filename)
	filename = SanitizeFilename(filename)

	f, err := os.Create(filepath.Join(uploadDir, filename))
	if err != nil {
		log.Debug().Err(err).Msg("Error creating direct media file")
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		log.Debug().Err(err).Msg("Error saving direct media file")
		return "", err
	}

	return filename, nil
}

func directMediaFilename(sourceURL string, contentDisposition string, contentType string) string {
	if contentDisposition != "" {
		if _, params, err := mime.ParseMediaType(contentDisposition); err == nil && params["filename"] != "" {
			return params["filename"]
		}
	}

	if parsedURL, err := url.Parse(sourceURL); err == nil {
		if base := filepath.Base(parsedURL.Path); base != "." && base != "/" && base != "" {
			return base
		}
	}

	ext := ".mp4"
	if strings.Contains(contentType, "audio/") {
		ext = ".mp3"
	}
	return "media" + ext
}

func lastNonEmptyLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func SendTranscriptionRequest(t *models.Transcription, body *bytes.Buffer, writer *multipart.Writer) (*models.WhisperResult, error) {
	url := fmt.Sprintf("http://%v/transcribe?model_size=%v&task=%v&language=%v&device=%v", os.Getenv("ASR_ENDPOINT"), t.ModelSize, t.Task, t.Language, t.Device)
	// Send transcription request to transcription service
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		log.Debug().Err(err).Msg("Error creating request to transcription service")
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Debug().Err(err).Msg("Error sending request")
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debug().Err(err).Msg("Error reading response body")
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		log.Debug().Msgf("Response from %v: %v", url, string(b))
		log.Debug().Err(err).Msgf("Invalid response status %v:", resp.StatusCode)
		return nil, errors.New("invalid status")
	}

	var asrResponse *models.WhisperResult
	if err := json.Unmarshal(b, &asrResponse); err != nil {
		log.Debug().Err(err).Msg("Error decoding response")
		return nil, err
	}

	return asrResponse, nil

}
