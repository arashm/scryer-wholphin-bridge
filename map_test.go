package main

import "testing"

func ptr[T any](v T) *T { return &v }

func TestPermissions(t *testing.T) {
	for _, c := range []struct {
		req, auto []string
		want      int
	}{
		{[]string{"MOVIE", "SERIES", "ANIME"}, []string{"MOVIE"}, permRequest | permAutoApprove},
		{[]string{"MOVIE"}, nil, permRequestMovie},
		{[]string{"ANIME"}, nil, permRequestTV},
		{nil, []string{"MOVIE"}, 0}, // auto-approve without request is meaningless
	} {
		if got := permissions(c.req, c.auto); got != c.want {
			t.Errorf("permissions(%v,%v) = %d, want %d", c.req, c.auto, got, c.want)
		}
	}
}

func TestStatus(t *testing.T) {
	movie := title{Facet: "MOVIE", MediaFiles: []struct{}{{}}, ExternalIDs: []extID{{"tmdb", "1"}}}
	partial := title{Facet: "ANIME", EpisodesOwned: ptr(54), EpisodesTotal: ptr(57), ExternalIDs: []extID{{"tmdb", "2"}},
		Collections: []collection{
			{"SPECIALS", "0", ptr(0), ptr(58)},
			{"SEASON", "1", ptr(24), ptr(24)},
			{"SEASON", "3", ptr(6), ptr(9)},
			{"SEASON", "4", ptr(0), ptr(12)},
		}}
	sel := &monitorSelection{[]int{4}}
	reqs := []scryerRequest{
		{ID: "r1", Facet: "ANIME", Status: "APPROVED", ExternalIDs: []extID{{"tmdb", "2"}}, Selection: sel},
		{ID: "r2", Facet: "MOVIE", Status: "PENDING", ExternalIDs: []extID{{"tmdb", "3"}}},
		{ID: "r3", Facet: "MOVIE", Status: "CANCELED", ExternalIDs: []extID{{"tmdb", "4"}}},
		{ID: "r4", Facet: "SERIES", Status: "PENDING", ExternalIDs: []extID{{"tmdb", "1"}}}, // tmdb ids collide across kinds
	}
	lib := newLibrary([]title{movie, partial}, reqs, 7)

	if mi := lib.mediaInfo("movie", 1); mi == nil || mi.Status != 5 || len(mi.Requests) != 0 {
		t.Errorf("owned movie: %+v", mi)
	}
	mi := lib.mediaInfo("tv", 2)
	if mi == nil || mi.Status != 4 {
		t.Fatalf("partial anime as tv: %+v", mi)
	}
	got := map[int]int{}
	for _, s := range mi.Seasons {
		got[s.SeasonNumber] = s.Status
	}
	if got[1] != 5 || got[3] != 4 || got[4] != 3 || len(got) != 3 {
		t.Errorf("seasons = %v", got)
	}
	if mi := lib.mediaInfo("movie", 3); mi == nil || mi.Status != 2 || mi.Requests[0].Status != 1 || mi.Requests[0].RequestedBy.ID != 7 {
		t.Errorf("pending movie: %+v", mi)
	}
	if mi := lib.mediaInfo("movie", 4); mi != nil {
		t.Errorf("canceled request should be requestable again: %+v", mi)
	}
	if mi := lib.mediaInfo("tv", 1); mi == nil || mi.Status != 2 {
		t.Errorf("tv 1 must not see movie 1: %+v", mi)
	}
}

func TestImages(t *testing.T) {
	if got := imagePath(ptr("/images/media/ab12/w250")); got != "/images/media/ab12" {
		t.Errorf("imagePath = %q", got)
	}
	if got := imagePath(ptr("/images/media/../evil/w250")); got != "" {
		t.Errorf("imagePath accepted %q", got)
	}
	scryerPublicURL = "https://s"
	if got := backdropPath("movie", 7, ptr("/images/media/cd34/w1280"), ptr("/images/media/ab12/w250")); got != "/images/media/cd34-ab12-m7" {
		t.Errorf("backdropPath = %q", got)
	}
	if got := backdropPath("tv", 0, nil, ptr("/images/media/ab12/w250")); got != "/images/media/-ab12-" {
		t.Errorf("backdropPath without backdrop or id = %q", got)
	}
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	tmdb := func(kind string, id int) string {
		if kind == "tv" && id == 9 {
			return "/bd.jpg"
		}
		return ""
	}
	const bd = "w1920_and_h1080_multi_faces"
	for _, c := range []struct {
		size, hash string
		exists     func(string) bool
		want       string
	}{
		{"w500", "ab12", no, "https://s/images/media/ab12/w250"},
		{bd, "cd34-ab12-t9", yes, "https://s/images/media/cd34/w1280"},
		{bd, "cd34-ab12-t9", no, "https://image.tmdb.org/t/p/w1280/bd.jpg"}, // Scryer 404s, TMDB has one
		{bd, "cd34-ab12-m9", no, "https://s/images/media/ab12/original"},    // neither has one: poster
		{bd, "cd34-ab12", no, "https://s/images/media/ab12/original"},       // old path format still works
		{bd, "-ab12-t9", no, "https://image.tmdb.org/t/p/w1280/bd.jpg"},
		{bd, "--t9", no, "https://image.tmdb.org/t/p/w1280/bd.jpg"},
		{bd, "cd34", no, "https://s/images/media/cd34/w1280"}, // no fallback, so don't bother checking
		{bd, "cd34--m1", no, ""},
		{"w500", "x.evil.com", yes, ""},
		{"w500", "-ab12", yes, ""},
		{bd, "ab12-cd34-x9", yes, ""},
		{bd, "-", yes, ""},
	} {
		if got := imageRedirect(c.size, c.hash, c.exists, tmdb); got != c.want {
			t.Errorf("imageRedirect(%q, %q) = %q, want %q", c.size, c.hash, got, c.want)
		}
	}
}

func TestCached(t *testing.T) {
	calls := 0
	f := func(ok bool) func() (string, bool) { return func() (string, bool) { calls++; return "v", ok } }
	cached("t:outage", f(false))
	cached("t:outage", f(false))
	cached("t:ok", f(true))
	if v := cached("t:ok", f(true)); v != "v" || calls != 3 {
		t.Errorf("v=%q calls=%d, want outages retried and definitive answers remembered", v, calls)
	}
}

func TestJellyfinID(t *testing.T) {
	tt := &title{PlaybackLinks: []struct{ Href string }{{"https://jf/web/index.html#!/details?id=0123456789abcdef0123456789ABCDEF&context=home"}}}
	if got := jellyfinID(tt); got != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Errorf("jellyfinID = %q", got)
	}
}

func TestRatingScore(t *testing.T) {
	rs := []rating{{"tomatoes", ptr(36.4), 3.6}, {"rogerebert", nil, 2.5}}
	if v := ratingScore(rs, "tomatoes"); v == nil || *v != 36 {
		t.Errorf("tomatoes = %v", v)
	}
	if v := ratingScore(rs, "rogerebert"); v == nil || *v != 25 {
		t.Errorf("fallback to normalized = %v", v)
	}
	if v := ratingScore(rs, "popcorn"); v != nil {
		t.Errorf("missing source = %v", *v)
	}
}

func TestHashIDStable(t *testing.T) {
	if hashID("f3018f03-abcb-4351-ac58-d63f88886e71") != hashID("f3018f03-abcb-4351-ac58-d63f88886e71") || hashID("a") < 0 {
		t.Error("hashID not stable/positive")
	}
}

func TestCreditResults(t *testing.T) {
	rs := creditResults([]tmdbCredit{
		{ID: 1, MediaType: "movie", Title: "M", Character: "Hero", PosterPath: "/p.jpg", ReleaseDate: "2001-02-03"},
		{ID: 2, MediaType: "tv", Name: "S", Job: "Director"},
		{ID: 3, MediaType: "movie", Adult: true},
		{ID: 4, MediaType: "person"},
	})
	if len(rs) != 2 || rs[0].Character != "Hero" || rs[0].PosterPath != "/p.jpg" || rs[1].Job != "Director" || rs[1].MediaType != "tv" {
		t.Errorf("creditResults = %+v", rs)
	}
	for s, want := range map[string]bool{"w500": true, "original": true, "w1920_and_h1080_multi_faces": true, "w500/../x": false, "": false} {
		if tmdbSizeRe.MatchString(s) != want {
			t.Errorf("tmdb size %q", s)
		}
	}
	for f, want := range map[string]bool{"abc123.jpg": true, "a.jpg?x": false, "..jpg": false, "a/b.jpg": false} {
		if tmdbFileRe.MatchString(f) != want {
			t.Errorf("tmdb file %q", f)
		}
	}
}
