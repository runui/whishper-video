package models

type Translation struct {
	SourceLanguage string        `json:"sourceLanguage"`
	TargetLanguage string        `json:"targetLanguage"`
	Status         int           `json:"translationStatus"`
	Result         WhisperResult `json:"result"`
	Engine         string        `bson:"engine,omitempty" json:"engine,omitempty"`
}
