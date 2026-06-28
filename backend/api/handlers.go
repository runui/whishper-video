package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"codeberg.org/pluja/whishper/models"
	"codeberg.org/pluja/whishper/utils"
)

type embyWebhookPayload struct {
	Event string `json:"Event"`
	Item  struct {
		Path string `json:"Path"`
	} `json:"Item"`
}

type localFileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

func (s *Server) handleGetAllTranscriptions(c *fiber.Ctx) error {
	transcriptions := s.Db.GetAllTranscriptions()

	// Convert the transcriptions to JSON.
	json, err := json.Marshal(transcriptions)
	if err != nil {
		// 503 On vacation!
		return fiber.NewError(fiber.StatusServiceUnavailable, "On vacation!")
	}

	// Write the JSON to the response body.
	c.Set("Content-Type", "application/json")
	c.Write(json)
	return nil
}

func (s *Server) handleGetTranscriptionById(c *fiber.Ctx) error {
	id := c.Params("id")
	t := s.Db.GetTranscription(id)
	if t == nil {
		log.Warn().Msgf("Transcription with id %v not found", id)
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	// Convert the transcription to JSON.
	json, err := json.Marshal(t)
	if err != nil {
		// 503 On vacation!
		return fiber.NewError(fiber.StatusServiceUnavailable, "On vacation!")
	}

	// Write the JSON to the response body.
	c.Set("Content-Type", "application/json")
	c.Write(json)
	return nil
}

func (s *Server) handleListLocalFiles(c *fiber.Ctx) error {
	path := c.Query("path", "/")
	if !filepath.IsAbs(path) {
		return fiber.NewError(fiber.StatusBadRequest, "path must be absolute")
	}

	info, err := os.Stat(path)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "path not found")
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fiber.NewError(fiber.StatusForbidden, "path cannot be read")
	}

	result := make([]localFileEntry, 0, len(entries))
	for _, entry := range entries {
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}
		entryPath := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			entryPath += string(os.PathSeparator)
		}
		result = append(result, localFileEntry{
			Name:  entry.Name(),
			Path:  entryPath,
			IsDir: entry.IsDir(),
			Size:  entryInfo.Size(),
		})
	}

	return c.JSON(fiber.Map{
		"path":    path,
		"entries": result,
	})
}

// This function receives data from a form to create a new transcription.
// If the transcription is created successfully, it returns a 201 Created status code and
// broadcasts the new transcription to all ws clients.
func (s *Server) handlePostTranscription(c *fiber.Ctx) error {
	log.Debug().Msg("POST /api/transcriptions")
	var transcription models.Transcription

	localPath := c.FormValue("localPath")
	sourceUrl := c.FormValue("sourceUrl")

	// we get the filename from the form
	var filename string
	if localPath != "" {
		filename = filepath.Base(localPath)
	} else if sourceUrl == "" {
		// Get the form file from the request.
		file, err := c.FormFile("file")
		if err != nil {
			log.Error().Err(err).Msg("Error getting file field from the form")
			return fiber.NewError(fiber.StatusBadRequest, "Bad request")
		}
		timeid := time.Now().Format("2006_01_02-150405000")
		filename = timeid + models.FileNameSeparator + file.Filename
		// if it's empty and there is no sourceurl we set a timestamp-based filename
		if filename == timeid+models.FileNameSeparator {
			filename = timeid + models.FileNameSeparator + time.Now().Format("2006_01_02-150405")
		}

		// Save the file to the uploads directory.
		err = c.SaveFile(file, fmt.Sprintf("%v/%v", os.Getenv("UPLOAD_DIR"), filename))
		if err != nil {
			log.Error().Err(err).Msgf("Error saving the form file to disk into %v", os.Getenv("UPLOAD_DIR"))
			return fiber.NewError(fiber.StatusInternalServerError, "Internal server error")
		}
	}

	// Parse the body into the transcription struct.
	transcription.Language = c.FormValue("language")
	transcription.ModelSize = c.FormValue("modelSize")
	transcription.FileName = filename
	transcription.LocalPath = localPath
	transcription.Status = models.TranscriptionStatusPending
	transcription.Task = "transcribe"
	transcription.SourceUrl = sourceUrl
	transcription.SkipWhisper = c.FormValue("skipWhisper") == "true"
	transcription.Device = c.FormValue("device")
	if transcription.Device != "cpu" && transcription.Device != "cuda" {
		log.Warn().Msgf("Device %v not supported, using cpu", transcription.Device)
		transcription.Device = "cpu"
	}

	log.Debug().Msgf("Transcription: %+v", transcription)
	// Save transcription to database
	res, err := s.Db.NewTranscription(&transcription)
	if err != nil {
		log.Error().Err(err).Msg("Error saving transcription to database")
		return fiber.NewError(fiber.StatusInternalServerError, "Internal server error")
	}

	// Broadcast transcription to websocket clients
	s.BroadcastTranscription(res)
	s.NewTranscriptionCh <- true

	// Convert the transcription to JSON.
	json, err := json.Marshal(res)
	if err != nil {
		// 503 On vacation!
		return fiber.NewError(fiber.StatusServiceUnavailable, "On vacation!")
	}

	// Write the JSON to the response body.
	c.Set("Content-Type", "application/json")
	c.Write(json)
	return nil
}

func (s *Server) handleGetEmby(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"message": "You accessed this request incorrectly via a GET request. Configure Emby webhooks to POST multipart/form-data to /emby.",
	})
}

func (s *Server) handlePostEmby(c *fiber.Ctx) error {
	data := c.FormValue("data")
	if data == "" {
		return c.SendString("")
	}

	var payload embyWebhookPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		log.Error().Err(err).Msg("Error parsing Emby webhook data")
		return fiber.NewError(fiber.StatusBadRequest, "Bad request")
	}

	log.Debug().Msgf("Emby event detected is: %v", payload.Event)
	if payload.Event == "system.notificationtest" {
		log.Info().Msg("Emby test message received")
		return c.JSON(fiber.Map{"message": "Notification test received successfully!"})
	}

	if payload.Event != "library.new" && payload.Event != "playback.start" {
		return c.SendString("")
	}

	if payload.Item.Path == "" {
		return fiber.NewError(fiber.StatusBadRequest, "Missing Emby item path")
	}

	transcription := models.Transcription{
		Language:  embyDefault("EMBY_LANGUAGE", "auto"),
		ModelSize: embyDefault("EMBY_MODEL_SIZE", "small"),
		Status:    models.TranscriptionStatusPending,
		Task:      "transcribe",
		Device:    embyDevice(),
		FileName:  filepath.Base(payload.Item.Path),
		LocalPath: payload.Item.Path,
	}

	res, err := s.Db.NewTranscription(&transcription)
	if err != nil {
		log.Error().Err(err).Msg("Error saving Emby transcription to database")
		return fiber.NewError(fiber.StatusInternalServerError, "Internal server error")
	}

	s.BroadcastTranscription(res)
	s.NewTranscriptionCh <- true
	return c.SendString("")
}

func embyDefault(envName string, defaultValue string) string {
	if value := os.Getenv(envName); value != "" {
		return value
	}
	return defaultValue
}

func embyDevice() string {
	device := embyDefault("EMBY_DEVICE", "cpu")
	if device != "cpu" && device != "cuda" {
		return "cpu"
	}
	return device
}

func (s *Server) handleDeleteTranscription(c *fiber.Ctx) error {
	// First get the transcription from the database
	id := c.Params("id")
	t := s.Db.GetTranscription(id)
	if t == nil {
		log.Warn().Msgf("Transcription with id %v not found", id)
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	// Then delete uploaded/downloaded media from disk. Emby local paths point to a media
	// library file and must not be removed when deleting the transcription.
	if t.LocalPath == "" && t.FileName != "" {
		err := os.Remove(fmt.Sprintf("%v/%v", os.Getenv("UPLOAD_DIR"), t.FileName))
		if err != nil {
			log.Error().Err(err).Msgf("Error deleting file %v", t.FileName)
		}
	}

	// Finally delete the transcription from the database
	err := s.Db.DeleteTranscription(id)
	if err != nil {
		log.Error().Err(err).Msgf("Error deleting transcription %v", id)
		return fiber.NewError(fiber.StatusInternalServerError, "Internal server error")
	}

	// Return status deleted
	c.Status(fiber.StatusOK)
	return nil
}

func (s *Server) handlePatchTranscription(c *fiber.Ctx) error {
	var transcription models.Transcription
	// Parse the body into the transcription struct.
	err := json.Unmarshal(c.Body(), &transcription)
	if err != nil {
		log.Error().Err(err).Msg("Error parsing JSON body")
		return fiber.NewError(fiber.StatusBadRequest, "Bad request")
	}

	// Update the transcription in the database
	ut, err := s.Db.UpdateTranscription(&transcription)
	if err != nil {
		log.Error().Err(err).Msgf("Error updating transcription")
		if err.Error() == "no documents were modified" {
			return fiber.NewError(fiber.StatusNotModified, "Not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "Internal server error")
	}

	// Write the JSON to the response body.
	s.BroadcastTranscription(ut)

	// Return status ok
	json, err := json.Marshal(&ut)
	if err != nil {
		// 503 On vacation!
		return fiber.NewError(fiber.StatusInternalServerError, "Error parsing json!")
	}

	c.Status(fiber.StatusOK)
	c.Write(json)
	return nil
}

func (s *Server) handleTranslate(c *fiber.Ctx) error {
	id := c.Params("id")
	targetLang := c.Params("target")
	start := time.Now()

	log.Info().Str("id", id).Str("targetLang", targetLang).Msg("Starting LibreTranslate translation")

	transcription := s.Db.GetTranscription(id)

	// Set status as translating
	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	err := transcription.Translate(targetLang)
	if err != nil {
		log.Error().Err(err).Str("id", id).Dur("duration", time.Since(start)).Msg("LibreTranslate translation failed")
		transcription.Status = models.TranscriptionStatusTranslationError
		transcription.TranslationError = err.Error()
		s.Db.UpdateTranscription(transcription)
		s.BroadcastTranscription(transcription)
		return err
	}

	// Set as done
	transcription.Status = models.TranscriptionStatusDone
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	log.Info().Str("id", id).Str("targetLang", targetLang).Dur("duration", time.Since(start)).Msg("LibreTranslate translation completed")
	return nil
}

func (s *Server) handleTranslateSubtitleTrack(c *fiber.Ctx) error {
	id := c.Params("id")
	trackID := c.Params("track")
	targetLang := c.Params("target")
	start := time.Now()

	log.Info().Str("id", id).Str("track", trackID).Str("targetLang", targetLang).Msg("Starting LibreTranslate subtitle track translation")

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	trackIndex, ok := findLLMSubtitleTrack(transcription.SubtitleTracks, trackID)
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "Subtitle track not found")
	}

	track := &transcription.SubtitleTracks[trackIndex]
	for _, translation := range track.Translations {
		if translation.TargetLanguage == targetLang && translation.Engine != "llm" {
			log.Warn().Str("id", id).Str("targetLang", targetLang).Msg("Translation already exists")
			return fiber.NewError(fiber.StatusBadRequest, "translation already exists")
		}
	}

	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	translation, err := models.TranslateWhisperResult(track.Result, track.Language, targetLang)
	if err != nil {
		log.Error().Err(err).Str("id", id).Str("track", trackID).Dur("duration", time.Since(start)).Msg("LibreTranslate subtitle track translation failed")
		transcription.Status = models.TranscriptionStatusTranslationError
		transcription.TranslationError = err.Error()
		s.Db.UpdateTranscription(transcription)
		s.BroadcastTranscription(transcription)
		return err
	}

	translation.Engine = "libretranslate"
	track.Translations = append(track.Translations, translation)
	transcription.Status = models.TranscriptionStatusDone
	ut, err := s.Db.UpdateTranscription(transcription)
	if err != nil {
		return err
	}
	s.BroadcastTranscription(ut)
	log.Info().Str("id", id).Str("track", trackID).Str("targetLang", targetLang).Dur("duration", time.Since(start)).Msg("LibreTranslate subtitle track translation completed")
	return nil
}

func (s *Server) handleLLMTranslateSubtitleTrack(c *fiber.Ctx) error {
	id := c.Params("id")
	trackID := c.Params("track")
	start := time.Now()

	var req models.LLMTranslationRequest
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Bad request")
		}
	}
	if req.TargetLanguage == "" {
		return fiber.NewError(fiber.StatusBadRequest, "targetLanguage is required")
	}

	log.Info().Str("id", id).Str("track", trackID).Str("targetLang", req.TargetLanguage).Str("model", req.Model).Msg("Starting LLM subtitle track translation")

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	trackIndex, ok := findLLMSubtitleTrack(transcription.SubtitleTracks, trackID)
	if !ok {
		log.Error().Str("id", id).Str("track", trackID).Msg("Subtitle track not found for LLM translation")
		return fiber.NewError(fiber.StatusNotFound, "Subtitle track not found")
	}

	track := &transcription.SubtitleTracks[trackIndex]
	for _, translation := range track.Translations {
		if translation.TargetLanguage == req.TargetLanguage && translation.Engine == "llm" {
			log.Warn().Str("id", id).Str("targetLang", req.TargetLanguage).Msg("LLM translation already exists")
			return fiber.NewError(fiber.StatusBadRequest, "translation already exists")
		}
	}

	s.enrichLLMContext(transcription, &req)

	transcription.TranslationError = ""
	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	ctx, cancel := context.WithCancel(context.Background())
	models.RegisterTranslationCancel(id, cancel)

	var lastProgressUpdate time.Time
	onProgress := func(total, completed, currentSegment int, status string) {
		transcription.TranslationProgress = &models.TranslationProgress{
			Total: total, Completed: completed, CurrentSegment: currentSegment, CurrentStatus: status,
		}
		now := time.Now()
		if now.Sub(lastProgressUpdate) > time.Second || completed >= total {
			s.BroadcastTranscription(transcription)
			lastProgressUpdate = now
		}
	}

	go func() {
		defer models.UnregisterTranslationCancel(id)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.Error().Str("id", id).Interface("panic", r).Msg("LLM subtitle track translation panicked")
				transcription.Status = models.TranscriptionStatusTranslationError
				transcription.TranslationError = fmt.Sprintf("internal error: %v", r)
				transcription.TranslationProgress = nil
				s.Db.UpdateTranscription(transcription)
				s.BroadcastTranscription(transcription)
			}
		}()

		translation, err := models.TranslateWhisperResultWithLLM(ctx, track.Result, track.Language, req, onProgress)
		transcription.TranslationProgress = nil
		if err != nil {
			log.Error().Err(err).Str("id", id).Str("track", trackID).Str("targetLang", req.TargetLanguage).Dur("duration", time.Since(start)).Msg("LLM subtitle track translation failed")
			transcription.Status = models.TranscriptionStatusTranslationError
			transcription.TranslationError = err.Error()
			s.Db.UpdateTranscription(transcription)
			s.BroadcastTranscription(transcription)
			return
		}

		track.Translations = append(track.Translations, translation)
		transcription.Status = models.TranscriptionStatusDone
		ut, err := s.Db.UpdateTranscription(transcription)
		if err != nil {
			log.Error().Err(err).Str("id", id).Msg("Failed to update transcription after translation")
			return
		}
		s.BroadcastTranscription(ut)
		log.Info().Str("id", id).Str("track", trackID).Str("targetLang", req.TargetLanguage).Dur("duration", time.Since(start)).Msg("LLM subtitle track translation completed")
	}()

	return c.JSON(transcription)
}

func (s *Server) handleLLMTranslate(c *fiber.Ctx) error {
	id := c.Params("id")
	start := time.Now()

	var req models.LLMTranslationRequest
	if len(c.Body()) > 0 {
		if err := json.Unmarshal(c.Body(), &req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Bad request")
		}
	}
	if req.TargetLanguage == "" {
		return fiber.NewError(fiber.StatusBadRequest, "targetLanguage is required")
	}

	log.Info().Str("id", id).Str("targetLang", req.TargetLanguage).Str("model", req.Model).Msg("Starting LLM translation")

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}
	if len(transcription.Result.Segments) == 0 {
		log.Error().Str("id", id).Msg("Transcription has no segments for LLM translation")
		transcription.Status = models.TranscriptionStatusError
		s.Db.UpdateTranscription(transcription)
		s.BroadcastTranscription(transcription)
		return fiber.NewError(fiber.StatusBadRequest, "transcription has no segments")
	}
	for _, translation := range transcription.Translations {
		if translation.TargetLanguage == req.TargetLanguage && translation.Engine == "llm" {
			log.Warn().Str("id", id).Str("targetLang", req.TargetLanguage).Msg("LLM translation already exists")
			return fiber.NewError(fiber.StatusBadRequest, "translation already exists")
		}
	}

	s.enrichLLMContext(transcription, &req)

	transcription.TranslationError = ""
	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	ctx, cancel := context.WithCancel(context.Background())
	models.RegisterTranslationCancel(id, cancel)

	var lastProgressUpdate time.Time
	onProgress := func(total, completed, currentSegment int, status string) {
		transcription.TranslationProgress = &models.TranslationProgress{
			Total: total, Completed: completed, CurrentSegment: currentSegment, CurrentStatus: status,
		}
		now := time.Now()
		if now.Sub(lastProgressUpdate) > time.Second || completed >= total {
			s.BroadcastTranscription(transcription)
			lastProgressUpdate = now
		}
	}

	go func() {
		defer models.UnregisterTranslationCancel(id)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.Error().Str("id", id).Interface("panic", r).Msg("LLM translation panicked")
				transcription.Status = models.TranscriptionStatusTranslationError
				transcription.TranslationError = fmt.Sprintf("internal error: %v", r)
				transcription.TranslationProgress = nil
				s.Db.UpdateTranscription(transcription)
				s.BroadcastTranscription(transcription)
			}
		}()

		translation, err := models.TranslateWhisperResultWithLLM(ctx, transcription.Result, transcription.Language, req, onProgress)
		transcription.TranslationProgress = nil
		if err != nil {
			log.Error().Err(err).Str("id", id).Str("targetLang", req.TargetLanguage).Dur("duration", time.Since(start)).Msg("LLM translation failed")
			transcription.Status = models.TranscriptionStatusTranslationError
			transcription.TranslationError = err.Error()
			s.Db.UpdateTranscription(transcription)
			s.BroadcastTranscription(transcription)
			return
		}

		transcription.Translations = append(transcription.Translations, translation)
		transcription.Status = models.TranscriptionStatusDone
		ut, err := s.Db.UpdateTranscription(transcription)
		if err != nil {
			log.Error().Err(err).Str("id", id).Msg("Failed to update transcription after translation")
			return
		}
		s.BroadcastTranscription(ut)
		log.Info().Str("id", id).Str("targetLang", req.TargetLanguage).Int("segments", len(translation.Result.Segments)).Dur("duration", time.Since(start)).Msg("LLM translation completed")
	}()

	return c.JSON(transcription)
}

func (s *Server) enrichLLMContext(transcription *models.Transcription, req *models.LLMTranslationRequest) {
	if transcription.LocalPath == "" {
		return
	}

	nfoContext := models.LoadNFOContext(transcription.LocalPath)
	if nfoContext != "" {
		req.AutoContext = nfoContext
		log.Info().Str("id", transcription.ID.Hex()).Str("localPath", transcription.LocalPath).Msg("Loaded NFO context for LLM translation")
	}

	mediaDir := filepath.Dir(transcription.LocalPath)
	allTranscriptions := s.Db.GetAllTranscriptions()
	var referencePairs []string
	for _, t := range allTranscriptions {
		if t.ID == transcription.ID {
			continue
		}
		if t.LocalPath == "" {
			continue
		}
		if filepath.Dir(t.LocalPath) != mediaDir {
			continue
		}
		for _, tr := range t.Translations {
			if tr.TargetLanguage == req.TargetLanguage && len(tr.Result.Segments) > 0 {
				maxRefs := 5
				count := 0
				for _, seg := range tr.Result.Segments {
					if seg.Text == "" {
						continue
					}
					var sourceText string
					for _, ts := range t.Result.Segments {
						if ts.ID == seg.ID || (ts.Start == seg.Start && ts.End == seg.End) {
							sourceText = ts.Text
							break
						}
					}
					if sourceText == "" {
						continue
					}
					referencePairs = append(referencePairs, fmt.Sprintf("Source: %s\nTarget: %s", sourceText, seg.Text))
					count++
					if count >= maxRefs {
						break
					}
				}
				if len(referencePairs) >= 10 {
					break
				}
			}
		}
		if len(referencePairs) >= 10 {
			break
		}
	}
	if len(referencePairs) > 0 {
		req.ReferenceExamples = "Reference translations from related episodes:\n" + strings.Join(referencePairs, "\n\n")
		log.Info().Str("id", transcription.ID.Hex()).Int("examples", len(referencePairs)).Msg("Loaded reference translations for LLM translation")
	}
}

func (s *Server) handleCancelTranslation(c *fiber.Ctx) error {
	id := c.Params("id")

	log.Info().Str("id", id).Msg("Translation cancellation requested")

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	if transcription.Status != models.TrannscriptionStatusTranslating && 
	   transcription.Status != models.TranscriptionStatusTranslationError &&
	   transcription.Status != models.TranscriptionStatusError {
		log.Warn().Str("id", id).Int("status", transcription.Status).Msg("Translation not in cancellable state")
		return fiber.NewError(fiber.StatusBadRequest, "translation is not in a cancellable state")
	}

	models.CancelTranslation(id)

	transcription.Status = models.TranscriptionStatusDone
	transcription.TranslationError = ""
	transcription.TranslationProgress = nil
	ut, err := s.Db.UpdateTranscription(transcription)
	if err != nil {
		return err
	}
	s.BroadcastTranscription(ut)
	return c.JSON(ut)
}

func findLLMSubtitleTrack(tracks []models.SubtitleTrack, trackID string) (int, bool) {
	if trackID != "auto" {
		return utils.FindSubtitleTrack(tracks, trackID)
	}

	for i, track := range tracks {
		language := strings.ToLower(track.Language)
		title := strings.ToLower(track.Title)
		if (language == "en" || language == "eng") && !strings.Contains(title, "sdh") && !strings.Contains(title, "cc") {
			return i, true
		}
	}
	for i, track := range tracks {
		language := strings.ToLower(track.Language)
		if language == "en" || language == "eng" {
			return i, true
		}
	}
	if len(tracks) > 0 {
		return 0, true
	}
	return 0, false
}

func (s *Server) handleExtractSubtitleTracks(c *fiber.Ctx) error {
	id := c.Params("id")
	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	mediaPath := fmt.Sprintf("%v/%v", os.Getenv("UPLOAD_DIR"), transcription.FileName)
	if transcription.LocalPath != "" {
		mediaPath = transcription.LocalPath
	}

	tracks, err := utils.ExtractSubtitleTracks(mediaPath)
	if err != nil {
		return err
	}
	utils.SortSubtitleTracks(tracks)
	transcription.SubtitleTracks = tracks
	if transcription.SkipWhisper {
		if len(tracks) > 0 {
			transcription.Result = tracks[0].Result
			transcription.Result.Text = "Subtitle tracks extracted. Select a subtitle track to download or translate."
			transcription.Result.Segments = []models.Segment{}
		} else {
			transcription.Result = models.WhisperResult{
				Language: transcription.Language,
				Text:     "No subtitle tracks found. Whisper transcription was skipped.",
				Segments: []models.Segment{},
			}
		}
	}

	ut, err := s.Db.UpdateTranscription(transcription)
	if err != nil {
		return err
	}
	s.BroadcastTranscription(ut)
	return c.JSON(ut)
}

type writeSubtitleRequest struct {
	Format    string `json:"format"`
	Content   string `json:"content"`
	Filename  string `json:"filename"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) handleWriteSubtitle(c *fiber.Ctx) error {
	id := c.Params("id")

	var req writeSubtitleRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request")
	}

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	if transcription.LocalPath == "" {
		return fiber.NewError(fiber.StatusBadRequest, "no local file path")
	}

	baseDir := filepath.Dir(transcription.LocalPath)
	var outputPath string
	if req.Filename != "" {
		outputPath = filepath.Join(baseDir, req.Filename)
	} else {
		ext := map[string]string{"srt": ".srt", "vtt": ".vtt", "txt": ".txt", "json": ".json"}[req.Format]
		lang := transcription.Language
		if lang == "" {
			lang = transcription.Result.Language
		}
		baseName := strings.TrimSuffix(transcription.LocalPath, filepath.Ext(transcription.LocalPath))
		outputPath = baseName + "_" + lang + ext
	}

	if _, err := os.Stat(outputPath); err == nil && !req.Overwrite {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "file already exists",
			"path":  outputPath,
		})
	}

	if err := os.WriteFile(outputPath, []byte(req.Content), 0644); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to write file: "+err.Error())
	}

	log.Info().Str("id", id).Str("path", outputPath).Str("format", req.Format).Msg("Subtitle file written to local path")
	return c.JSON(fiber.Map{"path": outputPath})
}
