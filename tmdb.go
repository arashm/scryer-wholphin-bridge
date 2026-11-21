package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// tmdbKey is optional. Scryer has no person data, so actor pages need TMDB.
// Either a v3 API key or a v4 read access token works.
var tmdbKey = os.Getenv("TMDB_API_KEY")

func tmdbGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.themoviedb.org/3"+path+"?language=en-US", nil)
	if err != nil {
		return err
	}
	if strings.HasPrefix(tmdbKey, "eyJ") { // v4 tokens are JWTs
		req.Header.Set("Authorization", "Bearer "+tmdbKey)
	} else {
		req.URL.RawQuery += "&api_key=" + url.QueryEscape(tmdbKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL may hold the key, keep it out of logs and responses
		}
		return fmt.Errorf("tmdb: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		return &httpError{http.StatusNotFound, "not found on TMDB"}
	}
	return fmt.Errorf("tmdb: HTTP %d", resp.StatusCode)
}

type tmdbCredit struct {
	ID           int    `json:"id"`
	MediaType    string `json:"media_type"`
	Title        string `json:"title"`
	Name         string `json:"name"`
	Character    string `json:"character"`
	Job          string `json:"job"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	Adult        bool   `json:"adult"`
}

func creditResults(cs []tmdbCredit) []result {
	out := []result{}
	for _, c := range cs {
		if c.Adult || (c.MediaType != "movie" && c.MediaType != "tv") {
			continue
		}
		out = append(out, result{ID: c.ID, MediaType: c.MediaType, Title: c.Title, Name: c.Name,
			Character: c.Character, Job: c.Job, Overview: c.Overview,
			PosterPath: c.PosterPath, BackdropPath: c.BackdropPath,
			ReleaseDate: c.ReleaseDate, FirstAirDate: c.FirstAirDate})
	}
	return out
}

func personCredits(ctx context.Context, r *http.Request, token string) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	empty := map[string]any{"id": id, "cast": []result{}, "crew": []result{}}
	if tmdbKey == "" {
		return empty, nil
	}
	var d struct{ Cast, Crew []tmdbCredit }
	if err := tmdbGet(ctx, "/person/"+strconv.Itoa(id)+"/combined_credits", &d); err != nil {
		return nil, err
	}
	cast, crew := creditResults(d.Cast), creditResults(d.Crew)
	all, err := fillMediaInfo(ctx, token, append(cast, crew...)) // one Scryer lookup for both
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "cast": all[:len(cast)], "crew": all[len(cast):]}, nil
}

var (
	tmdbSizeRe = regexp.MustCompile(`^(original|[wh][0-9]+|w[0-9]+_and_h[0-9]+_multi_faces)$`)
	tmdbFileRe = regexp.MustCompile(`^[A-Za-z0-9]+\.(jpg|png|svg)$`)
)

// tmdbImage redirects TMDB image paths (from person credits) to TMDB's image CDN.
func tmdbImage(w http.ResponseWriter, r *http.Request) {
	size, file := r.PathValue("size"), r.PathValue("file")
	if !tmdbSizeRe.MatchString(size) || !tmdbFileRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "https://image.tmdb.org/t/p/"+size+"/"+file, http.StatusFound)
}

// tmdbBackdrop looks up a title's backdrop file on TMDB, or "" without a key or backdrop.
func tmdbBackdrop(ctx context.Context) func(kind string, id int) string {
	return func(kind string, id int) string {
		if tmdbKey == "" {
			return ""
		}
		return cached("tmdb:"+kind+":"+strconv.Itoa(id), func() (string, bool) {
			var d struct {
				BackdropPath string `json:"backdrop_path"`
			}
			err := tmdbGet(ctx, "/"+kind+"/"+strconv.Itoa(id), &d)
			var he *httpError
			if errors.As(err, &he) && he.code == http.StatusNotFound {
				return "", true
			}
			if err != nil || !tmdbFileRe.MatchString(strings.TrimPrefix(d.BackdropPath, "/")) {
				return "", err == nil
			}
			return d.BackdropPath, true
		})
	}
}
