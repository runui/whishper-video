package models

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	ltr "github.com/snakesel/libretranslate"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Transcription struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Status         int                `bson:"status" json:"status"`
	Language       string             `bson:"language" json:"language"`
	ModelSize      string             `bson:"modelSize" json:"modelSize"`
	Task           string             `bson:"task" json:"task"`
	Device         string             `bson:"device" json:"device"`
	FileName       string             `bson:"fileName" json:"fileName"`
	LocalPath      string             `bson:"localPath" json:"localPath"`
	SourceUrl      string             `bson:"sourceUrl" json:"sourceUrl"`
	SkipWhisper    bool               `bson:"skipWhisper" json:"skipWhisper"`
	Result            WhisperResult      `bson:"result" json:"result"`
	Translations      []Translation      `bson:"translations" json:"translations"`
	SubtitleTracks    []SubtitleTrack    `bson:"subtitleTracks" json:"subtitleTracks"`
	TranslationError    string              `bson:"translationError" json:"translationError,omitempty"`
	TranslationProgress *TranslationProgress `bson:"translationProgress" json:"translationProgress,omitempty"`
}

type TranslationProgress struct {
	Total          int    `bson:"total" json:"total"`
	Completed      int    `bson:"completed" json:"completed"`
	CurrentSegment int    `bson:"currentSegment" json:"currentSegment"`
	CurrentStatus  string `bson:"currentStatus" json:"currentStatus"`
}

func libreTranslateLanguage(language string) string {
	switch language {
	case "zh", "chi", "zho":
		return "zh-Hans"
	case "eng":
		return "en"
	case "spa":
		return "es"
	case "fre", "fra":
		return "fr"
	case "ger", "deu":
		return "de"
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
	}
	return language
}

func (t *Transcription) Translate(target string) error {
	for _, translation := range t.Translations {
		if translation.TargetLanguage == target && translation.Engine != "llm" {
			log.Debug().Msgf("Translation for %v already exists!", target)
			return fmt.Errorf("translation for %v already exists", target)
		}
	}

	translation, err := TranslateWhisperResult(t.Result, t.Language, target)
	if err != nil {
		return err
	}
	translation.Engine = "libretranslate"
	t.Translations = append(t.Translations, translation)
	return nil
}

func TranslateWhisperResult(result WhisperResult, source string, target string) (Translation, error) {
	if source == "" || source == "auto" {
		source = result.Language
	}
	source = libreTranslateLanguage(source)
	target = libreTranslateLanguage(target)

	var translation Translation
	translation.SourceLanguage = source
	translation.TargetLanguage = target

	translate := ltr.New(ltr.Config{
		Url: fmt.Sprintf("http://%v", os.Getenv("TRANSLATION_ENDPOINT")),
	})

	trtext, err := translate.Translate(result.Text, translation.SourceLanguage, translation.TargetLanguage)
	if err != nil {
		log.Debug().Err(err).Msgf("Error translating text...")
		return Translation{}, err
	}
	translatedText := trtext

	translatedSegments := make([]Segment, len(result.Segments))
	copy(translatedSegments, result.Segments)
	for i, seg := range result.Segments {
		trtext, err := translate.Translate(seg.Text, translation.SourceLanguage, translation.TargetLanguage)
		if err != nil {
			log.Debug().Err(err).Msgf("Error translating segment text...")
			return Translation{}, err
		}
		translatedSegments[i].Text = trtext
		// Word-level data is lost, since we can't make sure that words will be in the same order and number as the final translation.
		// For example, if we translate "The big home" to Spanish, we could get "La casa grande", thus words changed order.
		translatedSegments[i].Words = []Word{}
	}

	translation.Result.Text = translatedText
	translation.Result.Segments = translatedSegments
	translation.Result.Language = translation.TargetLanguage
	translation.Result.Duration = result.Duration
	return translation, nil
}
