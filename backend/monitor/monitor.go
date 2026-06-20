package monitor

import (
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"codeberg.org/pluja/whishper/api"
	"codeberg.org/pluja/whishper/models"
	"codeberg.org/pluja/whishper/utils"
)

func StartMonitor(s *api.Server) {
	log.Info().Msg("Starting monitor!")
	go func() {
		recoverRunningTranscriptions(s)
		for {
			// Wait for new transcription to be added to the database
			// notification will be received through the NewTranscriptionCh channel
			<-s.NewTranscriptionCh
			pendingTranscriptions := s.Db.GetPendingTranscriptions()
			log.Debug().Msgf("Pending transcriptions: %v", len(pendingTranscriptions))
			for _, pt := range pendingTranscriptions {
				log.Debug().Msgf("Taking pending transcription %v", pt.ID)
				if pt.Status == models.TranscriptionStatusPending {
					err := transcribe(s, pt)
					if err != nil {
						log.Error().Err(err).Msg("Error transcribing")
						pt.Status = models.TranscriptionStatusError
						ut, err := s.Db.UpdateTranscription(pt)
						if err != nil {
							log.Error().Err(err).Msg("Error updating transcription")
						}
						s.BroadcastTranscription(ut)
						continue
					}
				}
			}
		}
	}()
}

func recoverRunningTranscriptions(s *api.Server) {
	runningTranscriptions := s.Db.GetRunningTranscriptions()
	log.Debug().Msgf("Recovering running transcriptions: %v", len(runningTranscriptions))
	for _, t := range runningTranscriptions {
		t.Status = models.TranscriptionStatusPending
		if _, err := s.Db.UpdateTranscription(t); err != nil {
			log.Error().Err(err).Msg("Error recovering running transcription")
			continue
		}
		s.BroadcastTranscription(t)
	}
}

func transcribe(s *api.Server, t *models.Transcription) error {
	// Update transcription status
	t.Status = models.TranscriptionStatusRunning
	log.Debug().Msgf("Updating transcription %v", t)
	_, err := s.Db.UpdateTranscription(t)
	if err != nil {
		log.Error().Err(err).Msg("Error updating transcription")
		return err
	}
	s.BroadcastTranscription(t)

	if t.SourceUrl != "" {
		// Download media
		if t.FileName == "" {
			fn, err := utils.DownloadMedia(t)
			if err != nil {
				log.Error().Err(err).Msg("Error downloading media")
				return err
			}
			t.FileName = fn
			if _, err := s.Db.UpdateTranscription(t); err != nil {
				log.Error().Err(err).Msg("Error updating downloaded filename")
				return err
			}
		}
		s.BroadcastTranscription(t)
	}

	if err := extractSubtitleTracks(s, t); err != nil {
		log.Debug().Err(err).Msg("Error extracting subtitle tracks")
	}

	if t.SkipWhisper {
		t.Result = subtitleOnlyResult(t)
		t.Translations = []models.Translation{}
		t.Status = models.TranscriptionStatusDone
		if _, err := s.Db.UpdateTranscription(t); err != nil {
			log.Error().Err(err).Msg("Error updating subtitle-only transcription")
			return err
		}
		s.BroadcastTranscription(t)
		return nil
	}

	// Send transcription request to transcription service
	res, err := utils.SendTranscriptionRequest(t, transcriptionFilePath(t))
	if err != nil {
		log.Error().Err(err).Msg("Error sending transcription request")
		return err
	}

	t.Result = *res
	t.Translations = []models.Translation{}
	t.Status = models.TranscriptionStatusDone
	_, err = s.Db.UpdateTranscription(t)
	if err != nil {
		log.Error().Err(err).Msg("Error updating transcription")
		return err
	}
	s.BroadcastTranscription(t)
	return nil
}

func subtitleOnlyResult(t *models.Transcription) models.WhisperResult {
	if len(t.SubtitleTracks) > 0 {
		result := t.SubtitleTracks[0].Result
		result.Text = "Subtitle tracks extracted. Select a subtitle track to download or translate."
		result.Segments = []models.Segment{}
		return result
	}

	return models.WhisperResult{
		Language: t.Language,
		Text:     "No subtitle tracks found. Whisper transcription was skipped.",
		Segments: []models.Segment{},
	}
}

func extractSubtitleTracks(s *api.Server, t *models.Transcription) error {
	tracks, err := utils.ExtractSubtitleTracks(transcriptionFilePath(t))
	if err != nil {
		return err
	}
	utils.SortSubtitleTracks(tracks)
	t.SubtitleTracks = tracks
	if _, err := s.Db.UpdateTranscription(t); err != nil {
		return err
	}
	s.BroadcastTranscription(t)
	return nil
}

func transcriptionFilePath(t *models.Transcription) string {
	if t.LocalPath != "" {
		return t.LocalPath
	}
	return filepath.Join(os.Getenv("UPLOAD_DIR"), t.FileName)
}
