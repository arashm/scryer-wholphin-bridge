package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

const pageSize = 20

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

// withStatus lets a handler return something other than 200.
type withStatus struct {
	code int
	v    any
}

type apiFunc func(ctx context.Context, r *http.Request, token string) (any, error)

// api wraps a handler: requires the Scryer API key (Wholphin sends it as X-Api-Key)
// and maps errors to HTTP statuses.
func api(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Api-Key")
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "missing X-Api-Key"})
			return
		}
		v, err := f(r.Context(), r, token)
		if err != nil {
			code := http.StatusBadGateway
			var he *httpError
			var se *scryerError
			switch {
			case errors.As(err, &he):
				code = he.code
			case errors.As(err, &se):
				code = se.status()
			}
			log.Printf("%s %s: %d %v", r.Method, r.URL.Path, code, err)
			writeJSON(w, code, map[string]string{"message": err.Error()})
			return
		}
		if s, ok := v.(withStatus); ok {
			if s.v == nil {
				w.WriteHeader(s.code)
				return
			}
			writeJSON(w, s.code, s.v)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func pathInt(r *http.Request, name string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n <= 0 {
		return 0, &httpError{http.StatusBadRequest, "invalid " + name}
	}
	return n, nil
}

func queryInt(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// ---- auth / settings ----

func authMe(ctx context.Context, r *http.Request, token string) (any, error) {
	var d struct {
		Me *struct{ ID, Username string }
		R  []struct{ Facet string }
		A  []struct{ Facet string }
	}
	err := gql(ctx, token, `{ me{id username} r: libraries(permission:REQUEST){facet} a: libraries(permission:AUTO_APPROVE_REQUESTS){facet} }`, nil, &d)
	if err != nil {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}
	facets := func(xs []struct{ Facet string }) (out []string) {
		for _, x := range xs {
			out = append(out, x.Facet)
		}
		return
	}
	return seerrUser{
		ID: hashID(d.Me.ID), Username: d.Me.Username, DisplayName: d.Me.Username,
		Permissions: permissions(facets(d.R), facets(d.A)),
	}, nil
}

func settingsPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"initialized": true, "cacheImages": true, "movie4kEnabled": false, "series4kEnabled": false})
}

// ---- library lookup shared by every list ----

// lookup fetches library titles + the user's requests for a set of tmdb ids in one call.
func lookup(ctx context.Context, token string, ids []int) (*library, error) {
	vals := []string{}
	for _, id := range ids {
		vals = append(vals, strconv.Itoa(id))
	}
	var d struct {
		Me *struct{ ID string }
		T  []title
		Q  []scryerRequest
	}
	q := `query($ids:[String!]!){ me{id} t: titlesByExternalIds(source:"tmdb", values:$ids){` + titleFields + `} q: myMediaRequests(limit:500){` + requestFields + `} }`
	if err := gql(ctx, token, q, map[string]any{"ids": vals}, &d); err != nil {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}
	return newLibrary(d.T, d.Q, hashID(d.Me.ID)), nil
}

func fillMediaInfo(ctx context.Context, token string, rs []result) ([]result, error) {
	ids := make([]int, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	if len(ids) == 0 {
		return rs, nil
	}
	lib, err := lookup(ctx, token, ids)
	if err != nil {
		return nil, err
	}
	for i := range rs {
		rs[i].MediaInfo = lib.mediaInfo(rs[i].MediaType, rs[i].ID)
	}
	return rs, nil
}

func newResult(k string, tmdb int, name string, year *int, overview, poster, backdrop *string) result {
	r := result{ID: tmdb, MediaType: k, Overview: str(overview), PosterPath: imagePath(poster), BackdropPath: backdropPath(backdrop, poster)}
	if k == "movie" {
		r.Title, r.ReleaseDate = name, yearDate(year)
	} else {
		r.Name, r.FirstAirDate = name, yearDate(year)
	}
	return r
}

func discoveryResults(items []discoveryItem) []result {
	out := []result{}
	for _, it := range items {
		k := ""
		switch it.TargetKind {
		case "movie":
			k = "movie"
		case "series", "anime":
			k = "tv"
		default:
			continue
		}
		tmdb := 0
		for _, x := range it.ExternalIDs {
			if x.Source == "tmdb" {
				tmdb, _ = strconv.Atoi(x.ID)
			}
		}
		if tmdb == 0 {
			continue // Wholphin addresses everything by tmdb id
		}
		out = append(out, newResult(k, tmdb, it.DisplayTitle, it.Year, it.Overview, it.PosterURL, it.BackgroundURL))
	}
	return out
}

const discoveryFields = `targetKind displayTitle year posterUrl backgroundUrl overview externalIds{source id}`

// ---- search / discover / similar ----

func search(ctx context.Context, r *http.Request, token string) (any, error) {
	pg := queryInt(r, "page", 1)
	empty := page{Page: pg, Results: []result{}}
	q := strings.TrimSpace(r.URL.Query().Get("query"))
	if q == "" || pg > 1 { // Scryer search has no paging: everything is on page 1
		return empty, nil
	}
	var d struct {
		S struct{ Movies, Series, Anime []searchItem }
	}
	err := gql(ctx, token, `query($q:String!){ s: searchMetadataMulti(query:$q, limit:20, language:"en"){ movies{tmdbId name year overview posterUrl} series{tmdbId name year overview posterUrl} anime{tmdbId name year overview posterUrl} } }`, map[string]any{"q": q}, &d)
	if err != nil {
		return nil, err
	}
	rs, seen := []result{}, map[string]bool{}
	add := func(k string, items []searchItem) {
		for _, it := range items {
			if it.TmdbID == nil || seen[k+strconv.Itoa(*it.TmdbID)] {
				continue
			}
			seen[k+strconv.Itoa(*it.TmdbID)] = true
			rs = append(rs, newResult(k, *it.TmdbID, it.Name, it.Year, it.Overview, it.PosterURL, nil))
		}
	}
	add("movie", d.S.Movies)
	add("tv", d.S.Anime)
	add("tv", d.S.Series)
	rs, err = fillMediaInfo(ctx, token, rs)
	if err != nil {
		return nil, err
	}
	return page{Page: 1, TotalPages: 1, TotalResults: len(rs), Results: rs}, nil
}

// discover merges one page of each discoveryItems input (GraphQL input fields, e.g. `sectionId:"x"`),
// keeping only kind ("movie"/"tv") unless it's "".
// ponytail: kind is filtered after paging, fine for the tiny upcoming sets it's used on.
func discover(kind string, inputs ...string) apiFunc {
	q := `query($o:Int!){`
	for i, in := range inputs {
		q += fmt.Sprintf(` d%d: discoveryItems(input:{%s, limit:%d, offset:$o, includePublic:true}){ totalCount items{%s} }`, i, in, pageSize, discoveryFields)
	}
	q += ` }`
	return func(ctx context.Context, r *http.Request, token string) (any, error) {
		pg := queryInt(r, "page", 1)
		off := (pg - 1) * pageSize
		var d map[string]struct {
			Items      []discoveryItem
			TotalCount int
		}
		if err := gql(ctx, token, q, map[string]any{"o": off}, &d); err != nil {
			return nil, err
		}
		var items []discoveryItem
		total, more := 0, false
		for i := range inputs {
			x := d[fmt.Sprint("d", i)]
			items = append(items, x.Items...)
			if len(x.Items) == pageSize { // Scryer's totalCount is only trustworthy when there's more
				total, more = total+max(x.TotalCount, off+pageSize), true
			}
		}
		rs, seen := []result{}, map[string]bool{}
		for _, it := range discoveryResults(items) {
			if k := it.MediaType + strconv.Itoa(it.ID); !seen[k] && (kind == "" || kind == it.MediaType) {
				seen[k] = true
				rs = append(rs, it)
			}
		}
		if !more {
			total = off + len(rs)
		}
		rs, err := fillMediaInfo(ctx, token, rs)
		if err != nil {
			return nil, err
		}
		return page{Page: pg, TotalPages: (total + pageSize - 1) / pageSize, TotalResults: total, Results: rs}, nil
	}
}

func similar(k string) apiFunc {
	return func(ctx context.Context, r *http.Request, token string) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if queryInt(r, "page", 1) > 1 {
			return page{Page: queryInt(r, "page", 1), Results: []result{}}, nil
		}
		key, facet := "tmdb:movie:"+strconv.Itoa(id), "MOVIE"
		if k == "tv" {
			key, facet = "tmdb:series:"+strconv.Itoa(id), "SERIES"
		}
		var d struct {
			N struct{ Items []discoveryItem }
		}
		err = gql(ctx, token, `query($k:String!,$f:MediaFacetValue!){ n: discoveryNeighbors(input:{targetKey:$k, facet:$f, limit:20}){ items{`+discoveryFields+`} } }`,
			map[string]any{"k": key, "f": facet}, &d)
		if err != nil {
			return nil, err
		}
		rs, err := fillMediaInfo(ctx, token, discoveryResults(d.N.Items))
		if err != nil {
			return nil, err
		}
		return page{Page: 1, TotalPages: 1, TotalResults: len(rs), Results: rs}, nil
	}
}

func emptyList(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, []any{}) }

// ---- details ----

type genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func genres(names []string) []genre {
	out := []genre{}
	for _, n := range names {
		out = append(out, genre{hashID(n), n})
	}
	return out
}

type metaMovie struct {
	Name            string
	Year            *int
	Overview        string
	PosterURL       *string
	BackgroundURL   *string
	TmdbReleaseDate *string
	RuntimeMinutes  int
	ImdbID          string
	Status          string
	GenresAndThemes []string
	ExternalRatings []rating
}

func movieDetails(ctx context.Context, r *http.Request, token string) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var d struct {
		Me *struct{ ID string }
		M  *metaMovie
		T  []title
		Q  []scryerRequest
	}
	err = gql(ctx, token, `query($id:Int!,$s:[String!]!){ me{id}
		m: metadataMovie(input:{tmdbId:$id, language:"en"}){ name year overview posterUrl backgroundUrl tmdbReleaseDate runtimeMinutes imdbId status genresAndThemes }
		t: titlesByExternalIds(source:"tmdb", values:$s){`+titleFields+`}
		q: myMediaRequests(facet:MOVIE, limit:500){`+requestFields+`} }`,
		map[string]any{"id": id, "s": []string{strconv.Itoa(id)}}, &d)
	if err != nil {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}
	m := d.M
	release := str(m.TmdbReleaseDate)
	if release == "" {
		release = yearDate(m.Year)
	}
	lib := newLibrary(d.T, d.Q, hashID(d.Me.ID))
	return map[string]any{
		"id": id, "title": m.Name, "originalTitle": m.Name, "overview": m.Overview,
		"posterPath": imagePath(m.PosterURL), "backdropPath": backdropPath(m.BackgroundURL, m.PosterURL),
		"releaseDate": release, "runtime": m.RuntimeMinutes, "imdbId": m.ImdbID, "status": m.Status,
		"genres": genres(m.GenresAndThemes), "mediaInfo": lib.mediaInfo("movie", id),
		"credits": map[string]any{"cast": []any{}, "crew": []any{}},
	}, nil
}

type metaSeries struct {
	Name            string
	Year            *int
	FirstAired      string
	Overview        string
	PosterURL       *string
	BackgroundURL   *string
	Network         string
	GenresAndThemes []string
	Seasons         []struct {
		Number int
		Label  string
	}
}

const seriesFields = `name year firstAired overview posterUrl backgroundUrl network genresAndThemes seasons{number label}`

func tvDetails(ctx context.Context, r *http.Request, token string) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var d struct {
		Me *struct{ ID string }
		S  *metaSeries
		T  []title
		Q  []scryerRequest
	}
	err = gql(ctx, token, `query($id:Int!,$s:[String!]!){ me{id}
		s: metadataSeries(input:{tmdbId:$id, language:"en", includeEpisodes:false}){`+seriesFields+`}
		t: titlesByExternalIds(source:"tmdb", values:$s){`+titleFields+`}
		q: myMediaRequests(limit:500){`+requestFields+`} }`,
		map[string]any{"id": id, "s": []string{strconv.Itoa(id)}}, &d)
	var se *scryerError
	if err != nil && !(errors.As(err, &se) && se.onlyOn("s")) {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}
	lib := newLibrary(d.T, d.Q, hashID(d.Me.ID))
	t := lib.titles["tv:"+strconv.Itoa(id)]
	s := d.S
	if s == nil { // metadata failed: fall back to the library title
		if t == nil {
			return nil, err
		}
		log.Printf("tv %d: metadata failed (%v), using library title", id, err)
		s = &metaSeries{Name: t.Name, Year: t.Year, Overview: str(t.Overview), PosterURL: t.PosterURL, BackgroundURL: t.BackgroundURL}
	}
	episodes := map[int]*int{}
	if t != nil {
		for _, c := range t.Collections {
			if n, err := strconv.Atoi(c.CollectionIndex); err == nil && c.CollectionType == "SEASON" {
				episodes[n] = c.EpisodesTotal
			}
		}
	}
	seasons := []season{}
	for _, x := range s.Seasons {
		seasons = append(seasons, season{ID: x.Number, SeasonNumber: x.Number, Name: x.Label, EpisodeCount: episodes[x.Number]})
	}
	first := s.FirstAired
	if first == "" {
		first = yearDate(s.Year)
	}
	networks := []genre{}
	if s.Network != "" {
		networks = append(networks, genre{hashID(s.Network), s.Network})
	}
	return map[string]any{
		"id": id, "name": s.Name, "originalName": s.Name, "overview": s.Overview,
		"posterPath": imagePath(s.PosterURL), "backdropPath": backdropPath(s.BackgroundURL, s.PosterURL),
		"firstAirDate": first, "genres": genres(s.GenresAndThemes), "networks": networks,
		"seasons": seasons, "numberOfSeason": len(seasons), "mediaInfo": lib.mediaInfo("tv", id),
		"credits": map[string]any{"cast": []any{}, "crew": []any{}},
	}, nil
}

func ratings(k string) apiFunc {
	return func(ctx context.Context, r *http.Request, token string) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		field := `m: metadataMovie(input:{tmdbId:$id})`
		if k == "tv" {
			field = `m: metadataSeries(input:{tmdbId:$id, includeEpisodes:false})`
		}
		var d struct {
			M *struct {
				Name            string
				Year            *int
				ExternalRatings []rating
			}
		}
		if err := gql(ctx, token, `query($id:Int!){ `+field+`{ name year externalRatings{source score normalized} } }`, map[string]any{"id": id}, &d); err != nil {
			return nil, err
		}
		out := map[string]any{"title": d.M.Name, "year": d.M.Year}
		if v := ratingScore(d.M.ExternalRatings, "tomatoes"); v != nil {
			out["criticsScore"] = *v
		}
		if v := ratingScore(d.M.ExternalRatings, "popcorn"); v != nil && k == "movie" {
			out["audienceScore"] = *v
		}
		return out, nil
	}
}

// ---- requests ----

func listRequests(ctx context.Context, r *http.Request, token string) (any, error) {
	take, skip := queryInt(r, "take", 50), queryInt(r, "skip", 0)
	var d struct {
		Me *struct{ ID, Username string }
		Q  []scryerRequest
	}
	err := gql(ctx, token, `query($l:Int!,$o:Int!){ me{id username} q: myMediaRequests(limit:$l, offset:$o){`+requestFields+`} }`,
		map[string]any{"l": take, "o": skip}, &d)
	if err != nil {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}
	lib := &library{meID: hashID(d.Me.ID)}
	rows := []mediaRequest{}
	for _, q := range d.Q {
		tmdb := tmdbOf(q.ExternalIDs)
		if q.Status == "CANCELED" || tmdb == 0 {
			continue
		}
		mr := lib.toRequest(q, tmdb)
		mr.RequestedBy.Username, mr.RequestedBy.DisplayName = d.Me.Username, d.Me.Username
		rows = append(rows, mr)
	}
	pages, total := skip/take+1, skip+len(d.Q)
	if len(d.Q) == take { // Scryer has no count: advertise one more page while pages are full
		pages++
		total += take
	}
	return map[string]any{
		"pageInfo": map[string]int{"page": skip/take + 1, "pages": pages, "results": total},
		"results":  rows,
	}, nil
}

func createRequest(ctx context.Context, r *http.Request, token string) (any, error) {
	var body struct {
		MediaType string `json:"mediaType"`
		MediaID   int    `json:"mediaId"`
		Seasons   []int  `json:"seasons"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MediaID <= 0 || (body.MediaType != "movie" && body.MediaType != "tv") {
		return nil, &httpError{http.StatusBadRequest, "need mediaType movie|tv and mediaId"}
	}
	var d struct {
		Me *struct{ ID string }
		M  *struct {
			Name           string
			Year           *int
			Overview       string
			RuntimeMinutes int
		}
		S *metaSeries
		T []title
		L []struct{ ID, Facet string }
	}
	vars := map[string]any{"id": body.MediaID, "s": []string{strconv.Itoa(body.MediaID)}}
	meta := `m: metadataMovie(input:{tmdbId:$id}){ name year overview runtimeMinutes }`
	if body.MediaType == "tv" {
		meta = `s: metadataSeries(input:{tmdbId:$id, language:"en", includeEpisodes:false}){` + seriesFields + `}`
	}
	err := gql(ctx, token, `query($id:Int!,$s:[String!]!){ me{id} `+meta+`
		t: titlesByExternalIds(source:"tmdb", values:$s){ name facet year externalIds{source value} }
		l: libraries(permission:REQUEST){ id facet } }`, vars, &d)
	var se *scryerError
	if err != nil && !(errors.As(err, &se) && se.onlyOn("m", "s")) {
		return nil, err
	}
	if d.Me == nil {
		return nil, &httpError{http.StatusUnauthorized, "invalid Scryer API key"}
	}

	in := map[string]any{"externalIds": []map[string]string{{"source": "tmdb", "value": strconv.Itoa(body.MediaID)}}}
	var t *title
	for i := range d.T {
		if kind(d.T[i].Facet) == body.MediaType {
			t = &d.T[i]
		}
	}
	facet := "MOVIE"
	switch {
	case body.MediaType == "movie" && d.M != nil:
		in["title"], in["year"], in["overview"], in["runtimeMinutes"] = d.M.Name, d.M.Year, d.M.Overview, d.M.RuntimeMinutes
	case body.MediaType == "tv" && d.S != nil:
		in["title"], in["year"], in["overview"] = d.S.Name, d.S.Year, d.S.Overview
		facet = "SERIES"
		for _, g := range d.S.GenresAndThemes {
			if strings.EqualFold(g, "anime") {
				facet = "ANIME"
			}
		}
	case t != nil:
		in["title"], in["year"] = t.Name, t.Year
	default:
		return nil, &httpError{http.StatusBadRequest, "no metadata for tmdb " + strconv.Itoa(body.MediaID)}
	}
	if t != nil {
		facet = t.Facet // a title already in a library keeps its facet
	}
	in["facet"] = facet
	for _, l := range d.L {
		if l.Facet == facet {
			in["libraryId"] = l.ID
			break
		}
	}
	if in["libraryId"] == nil {
		return nil, &httpError{http.StatusForbidden, "no " + facet + " library you can request in (accounts with Manage Titles can't request)"}
	}
	if body.MediaType == "tv" && len(body.Seasons) > 0 {
		in["requestedMonitorSelection"] = map[string]any{"seasonNumbers": body.Seasons}
	}

	reqID := "dry-run"
	if dryRun {
		var p struct {
			P struct {
				Outcome         string
				MetadataPartial bool
				FallbackReason  *string
			}
		}
		if err := gql(ctx, token, `query($in:SubmitMediaRequestInput!){ p: previewMyRequestDecision(input:$in){ outcome metadataPartial fallbackReason } }`, map[string]any{"in": in}, &p); err != nil {
			return nil, err
		}
		log.Printf("DRY RUN request %s %d %v: outcome=%s metadataPartial=%v", facet, body.MediaID, in["title"], p.P.Outcome, p.P.MetadataPartial)
	} else {
		var m struct{ R struct{ RequestID string } }
		if err := gql(ctx, token, `mutation($in:SubmitMediaRequestInput!){ r: submitMediaRequest(input:$in){ requestId } }`, map[string]any{"in": in}, &m); err != nil {
			return nil, err
		}
		reqID = m.R.RequestID
		log.Printf("request %s %d %v: %s", facet, body.MediaID, in["title"], reqID)
	}
	return withStatus{http.StatusCreated, mediaRequest{
		ID: hashID(reqID), Status: 1, Type: body.MediaType, Seasons: []season{},
		RequestedBy: seerrUser{ID: hashID(d.Me.ID)},
		Media:       &mediaInfo{ID: body.MediaID, TmdbID: body.MediaID, MediaType: body.MediaType, Requests: []mediaRequest{}},
	}}, nil
}

func deleteRequest(ctx context.Context, r *http.Request, token string) (any, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	var d struct{ Q []struct{ ID string } }
	if err := gql(ctx, token, `{ q: myMediaRequests(status:PENDING, limit:1000){ id } }`, nil, &d); err != nil {
		return nil, err
	}
	for _, q := range d.Q {
		if hashID(q.ID) == id {
			if err := gql(ctx, token, `mutation($id:ID!){ c: cancelMyMediaRequest(requestId:$id){ requestId } }`, map[string]any{"id": q.ID}, nil); err != nil {
				return nil, err
			}
			log.Printf("canceled request %s", q.ID)
			return withStatus{http.StatusNoContent, nil}, nil
		}
	}
	return nil, &httpError{http.StatusNotFound, "no pending request with that id"}
}

// ---- images ----

func imageProxy(w http.ResponseWriter, r *http.Request) {
	p := imageRedirect(r.PathValue("size"), r.PathValue("hash"), func(path string) bool {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, scryerURL+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if p == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, scryerPublicURL+p, http.StatusFound)
}
