package models

const (
	TranscriptionStatusPending      = 0
	TranscriptionStatusRunning      = 1
	TranscriptionStatusDone         = 2
	TrannscriptionStatusTranslating = 3
	TranscriptionStatusError        = -1
	TranscriptionStatusTranslationError = -2

	SourceTypeFile = "file"
	SourceTypeURL  = "url"

	FileNameSeparator = "_WHSHPR_"
)
