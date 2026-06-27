package models

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

type TVShowNFO struct {
	XMLName xml.Name `xml:"tvshow"`
	Title   string   `xml:"title"`
	Plot    string   `xml:"plot"`
	Year    string   `xml:"year"`
	Genre   []string `xml:"genre"`
	Studio  string   `xml:"studio"`
	Actors  []struct {
		Name  string `xml:"name"`
		Role  string `xml:"role"`
		Order int    `xml:"order"`
	} `xml:"actor"`
}

type EpisodeNFO struct {
	XMLName   xml.Name `xml:"episodedetails"`
	Title     string   `xml:"title"`
	ShowTitle string   `xml:"showtitle"`
	Season    int      `xml:"season"`
	Episode   int      `xml:"episode"`
	Plot      string   `xml:"plot"`
	Year      string   `xml:"year"`
	Aired     string   `xml:"aired"`
	Directors []string `xml:"director"`
	Actors    []struct {
		Name  string `xml:"name"`
		Role  string `xml:"role"`
		Order int    `xml:"order"`
	} `xml:"actor"`
}

func parseNFOFile[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read nfo file: %w", err)
	}

	var result T
	if err := xml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse nfo xml: %w", err)
	}
	return &result, nil
}

func ParseTVShowNFO(path string) (*TVShowNFO, error) {
	return parseNFOFile[TVShowNFO](path)
}

func ParseEpisodeNFO(path string) (*EpisodeNFO, error) {
	return parseNFOFile[EpisodeNFO](path)
}

func LoadNFOContext(localPath string) string {
	dir := filepath.Dir(localPath)
	var parts []string

	tvshowPath := filepath.Join(dir, "tvshow.nfo")
	if _, err := os.Stat(tvshowPath); err == nil {
		if show, err := ParseTVShowNFO(tvshowPath); err == nil {
			var showParts []string
			if show.Title != "" {
				showParts = append(showParts, fmt.Sprintf("Series: %s", show.Title))
			}
			if len(show.Genre) > 0 {
				showParts = append(showParts, fmt.Sprintf("Genre: %s", strings.Join(show.Genre, ", ")))
			}
			if show.Year != "" {
				showParts = append(showParts, fmt.Sprintf("Year: %s", show.Year))
			}
			if show.Plot != "" {
				showParts = append(showParts, fmt.Sprintf("Series overview: %s", truncateText(show.Plot, 500)))
			}
			if len(show.Actors) > 0 {
				var actorNames []string
				for _, a := range show.Actors {
					if a.Name != "" {
						n := a.Name
						if a.Role != "" {
							n += fmt.Sprintf(" (%s)", a.Role)
						}
						actorNames = append(actorNames, n)
					}
				}
				if len(actorNames) > 0 {
					showParts = append(showParts, fmt.Sprintf("Cast: %s", strings.Join(actorNames, ", ")))
				}
			}
			if len(showParts) > 0 {
				parts = append(parts, strings.Join(showParts, "\n"))
			}
		} else {
			log.Debug().Err(err).Str("path", tvshowPath).Msg("Failed to parse tvshow.nfo")
		}
	}

	baseName := strings.TrimSuffix(localPath, filepath.Ext(localPath))
	episodePath := baseName + ".nfo"
	if _, err := os.Stat(episodePath); err == nil {
		if ep, err := ParseEpisodeNFO(episodePath); err == nil {
			var epParts []string
			var epLabel string
			if ep.ShowTitle != "" {
				epLabel = ep.ShowTitle
			}
			if ep.Season > 0 && ep.Episode > 0 {
				if epLabel != "" {
					epLabel += " "
				}
				epLabel += fmt.Sprintf("S%02dE%02d", ep.Season, ep.Episode)
			}
			if ep.Title != "" {
				if epLabel != "" {
					epLabel += " - "
				}
				epLabel += ep.Title
			}
			if epLabel != "" {
				epParts = append(epParts, fmt.Sprintf("Episode: %s", epLabel))
			}
			if ep.Plot != "" {
				epParts = append(epParts, fmt.Sprintf("Plot: %s", truncateText(ep.Plot, 500)))
			}
			if ep.Aired != "" {
				epParts = append(epParts, fmt.Sprintf("Aired: %s", ep.Aired))
			}
			if len(ep.Directors) > 0 {
				epParts = append(epParts, fmt.Sprintf("Director: %s", strings.Join(ep.Directors, ", ")))
			}
			if len(epParts) > 0 {
				parts = append(parts, strings.Join(epParts, "\n"))
			}
		} else {
			log.Debug().Err(err).Str("path", episodePath).Msg("Failed to parse episode nfo")
		}
	}

	return strings.Join(parts, "\n\n")
}

func truncateText(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
