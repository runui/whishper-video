package api

import (
	"fmt"
	"os"
	"path/filepath"
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

// This function receives data from a form to create a new transcription.
// If the transcription is created successfully, it returns a 201 Created status code and
// broadcasts the new transcription to all ws clients.
func (s *Server) handlePostTranscription(c *fiber.Ctx) error {
	log.Debug().Msg("POST /api/transcriptions")
	var transcription models.Transcription

	// we get the filename from the from
	var filename string
	if c.FormValue("sourceUrl") == "" {
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
	transcription.Status = models.TranscriptionStatusPending
	transcription.Task = "transcribe"
	transcription.SourceUrl = c.FormValue("sourceUrl")
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

	transcription := s.Db.GetTranscription(id)

	// Set status as translating
	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	err := transcription.Translate(targetLang)
	if err != nil {
		log.Debug().Err(err).Msg("Error with translation")
		transcription.Status = models.TranscriptionStatusDone
		s.Db.UpdateTranscription(transcription)
		s.BroadcastTranscription(transcription)
		return err
	}

	// Set as done
	transcription.Status = models.TranscriptionStatusDone
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)
	return nil
}

func (s *Server) handleTranslateSubtitleTrack(c *fiber.Ctx) error {
	id := c.Params("id")
	trackID := c.Params("track")
	targetLang := c.Params("target")

	transcription := s.Db.GetTranscription(id)
	if transcription == nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	trackIndex, ok := utils.FindSubtitleTrack(transcription.SubtitleTracks, trackID)
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "Subtitle track not found")
	}

	track := &transcription.SubtitleTracks[trackIndex]
	for _, translation := range track.Translations {
		if translation.TargetLanguage == targetLang {
			return fiber.NewError(fiber.StatusBadRequest, "translation already exists")
		}
	}

	transcription.Status = models.TrannscriptionStatusTranslating
	s.Db.UpdateTranscription(transcription)
	s.BroadcastTranscription(transcription)

	translation, err := models.TranslateWhisperResult(track.Result, track.Language, targetLang)
	if err != nil {
		log.Debug().Err(err).Msg("Error translating subtitle track")
		transcription.Status = models.TranscriptionStatusDone
		s.Db.UpdateTranscription(transcription)
		s.BroadcastTranscription(transcription)
		return err
	}

	track.Translations = append(track.Translations, translation)
	transcription.Status = models.TranscriptionStatusDone
	ut, err := s.Db.UpdateTranscription(transcription)
	if err != nil {
		return err
	}
	s.BroadcastTranscription(ut)
	return nil
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
