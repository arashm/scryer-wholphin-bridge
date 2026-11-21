package main

import (
	"hash/fnv"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ---- Scryer shapes (only the fields we select) ----

type extID struct {
	Source string `json:"source"`
	Value  string `json:"value"`
}

type rating struct {
	Source     string   `json:"source"`
	Score      *float64 `json:"score"`
	Normalized float64  `json:"normalized"`
}

type collection struct {
	CollectionType  string `json:"collectionType"`
	CollectionIndex string `json:"collectionIndex"`
	EpisodesOwned   *int   `json:"episodesOwned"`
	EpisodesTotal   *int   `json:"episodesTotal"`
}

type title struct {
	Name          string                  `json:"name"`
	Facet         string                  `json:"facet"`
	Year          *int                    `json:"year"`
	Overview      *string                 `json:"overview"`
	PosterURL     *string                 `json:"posterUrl"`
	BackgroundURL *string                 `json:"backgroundUrl"`
	ExternalIDs   []extID                 `json:"externalIds"`
	EpisodesOwned *int                    `json:"episodesOwned"`
	EpisodesTotal *int                    `json:"episodesTotal"`
	MediaFiles    []struct{}              `json:"mediaFiles"`
	PlaybackLinks []struct{ Href string } `json:"playbackLinks"`
	Collections   []collection            `json:"collections"`
}

type scryerRequest struct {
	ID          string  `json:"id"`
	Facet       string  `json:"facet"`
	Status      string  `json:"status"`
	Title       string  `json:"title"`
	ExternalIDs []extID `json:"externalIds"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	Selection   *struct {
		SeasonNumbers []int `json:"seasonNumbers"`
	} `json:"requestedMonitorSelection"`
}

type discoveryItem struct {
	TargetKind    string  `json:"targetKind"`
	DisplayTitle  string  `json:"displayTitle"`
	Year          *int    `json:"year"`
	PosterURL     *string `json:"posterUrl"`
	BackgroundURL *string `json:"backgroundUrl"`
	Overview      *string `json:"overview"`
	ExternalIDs   []struct {
		Source string `json:"source"`
		ID     string `json:"id"`
	} `json:"externalIds"`
}

type searchItem struct {
	TmdbID    *int    `json:"tmdbId"`
	Name      string  `json:"name"`
	Year      *int    `json:"year"`
	Overview  *string `json:"overview"`
	PosterURL *string `json:"posterUrl"`
}

const titleFields = `name facet year overview posterUrl backgroundUrl externalIds{source value} episodesOwned episodesTotal mediaFiles{id} playbackLinks{href} collections{collectionType collectionIndex episodesOwned episodesTotal}`
const requestFields = `id facet status title externalIds{source value} createdAt updatedAt requestedMonitorSelection{seasonNumbers}`

// ---- Seerr shapes (only the fields Wholphin reads) ----

type mediaInfo struct {
	ID              int            `json:"id"`
	TmdbID          int            `json:"tmdbId"`
	Status          int            `json:"status"`
	Status4k        int            `json:"status4k"`
	MediaType       string         `json:"mediaType"`
	Seasons         []season       `json:"seasons,omitempty"`
	Requests        []mediaRequest `json:"requests"`
	JellyfinMediaID string         `json:"jellyfinMediaId,omitempty"`
}

type season struct {
	ID           int    `json:"id"`
	SeasonNumber int    `json:"seasonNumber"`
	Name         string `json:"name,omitempty"`
	EpisodeCount *int   `json:"episodeCount,omitempty"`
	Status       int    `json:"status,omitempty"`
}

type seerrUser struct {
	ID          int    `json:"id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Permissions int    `json:"permissions,omitempty"`
}

type mediaRequest struct {
	ID          int        `json:"id"`
	Status      int        `json:"status"`
	Type        string     `json:"type"`
	Is4k        bool       `json:"is4k"`
	Media       *mediaInfo `json:"media,omitempty"`
	RequestedBy seerrUser  `json:"requestedBy"`
	Seasons     []season   `json:"seasons"`
	CreatedAt   string     `json:"createdAt,omitempty"`
	UpdatedAt   string     `json:"updatedAt,omitempty"`
}

// result is MovieResult / TvResult / the search union, depending on which fields are set.
type result struct {
	ID           int        `json:"id"`
	MediaType    string     `json:"mediaType"`
	Title        string     `json:"title,omitempty"`
	Name         string     `json:"name,omitempty"`
	Overview     string     `json:"overview,omitempty"`
	PosterPath   string     `json:"posterPath,omitempty"`
	BackdropPath string     `json:"backdropPath,omitempty"`
	ReleaseDate  string     `json:"releaseDate,omitempty"`
	FirstAirDate string     `json:"firstAirDate,omitempty"`
	MediaInfo    *mediaInfo `json:"mediaInfo,omitempty"`
}

type page struct {
	Page         int      `json:"page"`
	TotalPages   int      `json:"totalPages"`
	TotalResults int      `json:"totalResults"`
	Results      []result `json:"results"`
}

// ---- helpers ----

// hashID turns a Scryer string id into the Int Seerr wants.
// ponytail: 31-bit hash, collisions are irrelevant at homelab scale.
func hashID(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32() & 0x7fffffff)
}

const (
	permRequest      = 32
	permAutoApprove  = 128
	permRequestMovie = 262144
	permRequestTV    = 524288
)

// permissions builds the Seerr bitmask from the facets of libraries the user can
// request in / auto-approve in. Never grants MANAGE_REQUESTS or REQUEST_ADVANCED.
func permissions(requestFacets, autoFacets []string) int {
	movie, tv := contains(requestFacets, "MOVIE"), contains(requestFacets, "SERIES") || contains(requestFacets, "ANIME")
	p := 0
	switch {
	case movie && tv:
		p = permRequest
	case movie:
		p = permRequestMovie
	case tv:
		p = permRequestTV
	}
	if len(autoFacets) > 0 && p != 0 {
		p |= permAutoApprove
	}
	return p
}

var hexRe = regexp.MustCompile(`^[0-9a-f]+$`)

// imagePath turns Scryer's "/images/media/<hash>/w250" into the "/images/media/<hash>" path
// Wholphin appends to /imageproxy/tmdb/t/p/<size>.
func imagePath(u *string) string {
	if u == nil {
		return ""
	}
	i := strings.Index(*u, "/images/media/")
	if i < 0 {
		return ""
	}
	h, _, _ := strings.Cut((*u)[i+len("/images/media/"):], "/")
	if !hexRe.MatchString(h) {
		return ""
	}
	return "/images/media/" + h
}

// imageRedirect maps a Seerr TMDB size + hash to the Scryer image URL, or "" if invalid.
func imageRedirect(size, hash string) string {
	if !hexRe.MatchString(hash) {
		return ""
	}
	variant := "w250"
	if strings.Contains(size, "_and_h") {
		variant = "w1280"
	}
	return scryerPublicURL + "/images/media/" + hash + "/" + variant
}

// jellyfinID pulls the 32-hex id= from a Jellyfin web link and dashes it into a UUID.
func jellyfinID(t *title) string {
	for _, l := range t.PlaybackLinks {
		u, err := url.Parse(l.Href)
		if err != nil {
			continue
		}
		// Jellyfin puts the query after the #! fragment.
		_, q, ok := strings.Cut(u.Fragment, "?")
		if !ok {
			q = u.RawQuery
		}
		v, _ := url.ParseQuery(q)
		id := strings.ToLower(v.Get("id"))
		if len(id) == 32 && hexRe.MatchString(id) {
			return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
		}
	}
	return ""
}

func tmdbOf(ids []extID) int {
	for _, x := range ids {
		if x.Source == "tmdb" {
			if n, err := strconv.Atoi(x.Value); err == nil {
				return n
			}
		}
	}
	return 0
}

// kind is the Seerr media type for a Scryer facet.
func kind(facet string) string {
	if facet == "MOVIE" {
		return "movie"
	}
	return "tv"
}

func ratingScore(rs []rating, source string) *int {
	for _, r := range rs {
		if r.Source != source {
			continue
		}
		v := r.Normalized * 10
		if r.Score != nil {
			v = *r.Score
		}
		n := int(math.Round(v))
		return &n
	}
	return nil
}

func requestStatus(s string) int {
	switch s {
	case "PENDING":
		return 1
	case "APPROVED":
		return 2
	case "REJECTED":
		return 3
	}
	return 0
}

func yearDate(y *int) string {
	if y == nil || *y <= 0 {
		return ""
	}
	// ponytail: Scryer lists only carry a year, so Jan 1 stands in for the release date.
	return strconv.Itoa(*y) + "-01-01"
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// library indexes titles and the user's own requests by "<kind>:<tmdb>".
type library struct {
	titles   map[string]*title
	requests map[string][]scryerRequest
	meID     int
}

func newLibrary(titles []title, reqs []scryerRequest, meID int) *library {
	l := &library{titles: map[string]*title{}, requests: map[string][]scryerRequest{}, meID: meID}
	for i := range titles {
		if id := tmdbOf(titles[i].ExternalIDs); id != 0 {
			l.titles[kind(titles[i].Facet)+":"+strconv.Itoa(id)] = &titles[i]
		}
	}
	for _, r := range reqs {
		if id := tmdbOf(r.ExternalIDs); id != 0 && (r.Status == "PENDING" || r.Status == "APPROVED") {
			k := kind(r.Facet) + ":" + strconv.Itoa(id)
			l.requests[k] = append(l.requests[k], r)
		}
	}
	return l
}

func (l *library) toRequest(r scryerRequest, tmdb int) mediaRequest {
	st := requestStatus(r.Status)
	out := mediaRequest{
		ID: hashID(r.ID), Status: st, Type: kind(r.Facet),
		RequestedBy: seerrUser{ID: l.meID},
		Seasons:     []season{},
		CreatedAt:   r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.Selection != nil {
		for _, n := range r.Selection.SeasonNumbers {
			out.Seasons = append(out.Seasons, season{ID: n, SeasonNumber: n, Status: st})
		}
	}
	out.Media = &mediaInfo{ID: tmdb, TmdbID: tmdb, MediaType: out.Type, Requests: []mediaRequest{}}
	return out
}

// mediaInfo is Seerr's availability block, or nil when the item is neither in the
// library nor actively requested (so Wholphin offers Request).
func (l *library) mediaInfo(k string, tmdb int) *mediaInfo {
	key := k + ":" + strconv.Itoa(tmdb)
	t, reqs := l.titles[key], l.requests[key]
	mi := &mediaInfo{ID: tmdb, TmdbID: tmdb, MediaType: k, Requests: []mediaRequest{}}
	for _, r := range reqs {
		mr := l.toRequest(r, tmdb)
		mr.Media = nil
		mi.Requests = append(mi.Requests, mr)
	}
	switch {
	case t != nil:
		mi.Status = titleStatus(t)
		mi.JellyfinMediaID = jellyfinID(t)
		mi.Seasons = seasonStatuses(t, reqs)
	case len(reqs) > 0:
		mi.Status = 2
		for _, r := range reqs {
			if r.Status == "APPROVED" {
				mi.Status = 3
			}
		}
		mi.Seasons = seasonStatuses(nil, reqs)
	default:
		return nil
	}
	return mi
}

func titleStatus(t *title) int {
	if t.Facet == "MOVIE" {
		if len(t.MediaFiles) > 0 {
			return 5
		}
		return 3
	}
	owned, total := deref(t.EpisodesOwned), deref(t.EpisodesTotal)
	switch {
	case total > 0 && owned >= total:
		return 5
	case owned > 0:
		return 4
	}
	return 3
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// seasonStatuses: owned seasons from Scryer collections, then requested seasons.
func seasonStatuses(t *title, reqs []scryerRequest) []season {
	byNum := map[int]int{}
	if t != nil {
		for _, c := range t.Collections {
			n, err := strconv.Atoi(c.CollectionIndex)
			if c.CollectionType != "SEASON" || err != nil || n == 0 {
				continue
			}
			owned, total := deref(c.EpisodesOwned), deref(c.EpisodesTotal)
			switch {
			case total > 0 && owned >= total:
				byNum[n] = 5
			case owned > 0:
				byNum[n] = 4
			}
		}
	}
	for _, r := range reqs {
		if r.Selection == nil {
			continue
		}
		st := 2
		if r.Status == "APPROVED" {
			st = 3
		}
		for _, n := range r.Selection.SeasonNumbers {
			if byNum[n] < st {
				byNum[n] = st
			}
		}
	}
	out := []season{}
	for n, st := range byNum {
		out = append(out, season{ID: n, SeasonNumber: n, Status: st})
	}
	return out
}
