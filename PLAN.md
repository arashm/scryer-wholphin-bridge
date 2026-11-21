# scryer-wholphin-bridge: plan

A small Go service that serves the part of the Seerr REST API (`/api/v1/...`) that Wholphin uses, and turns each call into a Scryer GraphQL call.

```
Wholphin (TV) --Seerr REST--> bridge (Go) --GraphQL--> Scryer (https://scryer.lab.amousavi.dev/graphql)
```

## Resolved: who can request in Scryer
Scryer source, `crates/scryer-domain/src/lib.rs:762`: `is_strictly_requestable = !MANAGE_TITLES && REQUEST`, and the same holds for auto-approve. **A user with Manage Titles can never request.** They're expected to add titles directly.
With `tv` set to `VIEW + REQUEST + AUTO_APPROVE_REQUESTS` on all three libraries (verified 2026-11-21), the **API key and a login session both work**: `libraries(permission:REQUEST)` lists all 3, and `previewMyRequestDecision` (title + tmdb id only) → `AUTO_APPROVE`, `metadataPartial:false`.
→ **Auth = API key.** The bridge must tell users to request with a non-manager account. If `/auth/me` finds no requestable library, it grants no request bits, so Wholphin hides the Request button instead of failing.

## Wholphin facts (verified in `Wholphin/app/src/main/java`)
- The client is generated from `app/src/main/seerr/seerr-api.yml`, with kotlinx JSON `ignoreUnknownKeys`, `explicitNulls=false`. Required fields: `User.id`, `MovieResult.id/mediaType`, `MediaRequest.id/status` (all **Int**).
- Images: `{base}/t/p/{size}{path}`, with `base = {bridge}/imageproxy/tmdb` when `/settings/public.cacheImages = true`.
- `mediaInfo.status`: 1 unknown, 2 pending, 3 processing, 4 partial, 5 available. `mediaInfo.jellyfinMediaId` (UUID) → opens the Jellyfin item.
- **Cancel** (`canUserCancelRequest`, `DiscoverMovieViewModel.kt:265`): needs permission bit `REQUEST` (32), `requestedBy.id == me.id`, and request status `< APPROVED` (pending only). Auto-approved requests can't be cancelled from the TV. That's acceptable.
- **No re-login on 401.** `seerrLogin` runs only at setup and in `UserSwitchListener` (startup / user switch). Cookies are in memory only.
- **Ratings** (`DiscoverItem.kt:130`): `/movie/{id}/ratings` → flat `criticsScore` (Int) + `audienceScore` (0–100, the app divides by 10). `/tv/{id}/ratings` → `criticsScore` only.
- **Requests page** (`SeerrRequestsPage.kt:79`): rows without `type` and `media.tmdbId` are skipped. The page calls `/movie|tv/{id}` once per row.
- `/service/radarr|sonarr` is only called with `REQUEST_ADVANCED`. The bridge never grants it.
- The **Fire TV flavor has `DISCOVER_ENABLED=false`**. Use the `default` or `appstore` APK.

## Scryer facts (verified live)
| Fact | Value |
|---|---|
| Auth header | `Authorization: Bearer <api key or session token>` |
| User id | UUID (`tv`) or 32-hex (`admin`) string |
| Libraries | `movie_default_library` MOVIE, `series_default_library` SERIES, `anime_default_library` ANIME; profile `1080p` |
| Login | `login(input:{username,password,persistSession:true})` → `{token, expiresAt (+3 days), mfaEnrollmentRequired, passwordChangeRequired}`. No refresh mutation exists |
| Search | `searchMetadataMulti(query, limit, language:"en")` → `movies`, `series`, `anime`. `tmdbId` is nullable |
| Movie | `metadataMovie(input:{tmdbId})` → overview, runtime, `tmdbReleaseDate`, poster, background, genres, `externalRatings{source score normalized}`. Sources seen: `tomatoes`, `popcorn`, `imdb`, `tmdb`, `metacritic`, `trakt`, … `score` is 0–100 and nullable (null for `rogerebert`). `normalized` is 0–10 and always set |
| Series | `metadataSeries(input:{tmdbId, includeEpisodes:false})` → `seasons{number label}`, `firstAired`, `network`. Without `includeEpisodes:false` some titles fail on bad episode dates (`INTERFACE_METADATA_DATE_INVALID`, tmdb 19566) |
| In library | `titlesByExternalIds(source:"tmdb", values:[…])`, batched. **tmdb ids collide between movies and TV, so filter on `facet`: movie → `MOVIE`; tv → `SERIES` *or* `ANIME`.** Select `mediaFiles{id}` only, because the full objects are large. Files: `mediaFiles` (count), `episodesOwned/episodesTotal`. `playbackLinks` is often empty even when files exist, so it's only used for the Jellyfin id |
| Discovery | `discoveryItems(input:{sectionId, limit, offset, includePublic:true})`. Sections: `trending_now`, `popular_movies`, `popular_series`, `upcoming_series`, … **`resolvedTitleId` and `ownedInInput` are always null/false** (0 of 100 `top_rated` items, though 4 are in the library), so availability comes from `titlesByExternalIds`. Items have `externalIds` → tmdb |
| Similar | `discoveryNeighbors(input:{targetKey:"tmdb:movie:<id>" or "tmdb:series:<id>", facet, limit})` works |
| Images | Public. Posters: `/w250`, `/original`. Backgrounds: `/w1280`, `/original` |
| Requests | `myMediaRequests(status, limit, offset)` (limit defaults to 200, max 1000) → UUID ids, `PENDING/APPROVED/REJECTED/CANCELED`, `externalIds`, `requestedMonitorSelection`. `submitMediaRequest` → `{requestId}`. `cancelMyMediaRequest(requestId)` |

## Auth
Wholphin **API-key mode**. Each user pastes their own Scryer API key (Profile → API keys, no expiry), and the bridge forwards `X-Api-Key` as `Authorization: Bearer`. The bridge stores nothing.
Match on `errors[].extensions.code` (verified live and in `scryer-interface-core/src/lib.rs:614`): `me` null or `AUTHENTICATION_REQUIRED` → `401`. `UNAUTHORIZED` → `403` (the detail is in `extensions.reason`, e.g. `AUTHORIZATION_ACTION_PERMISSION_DENIED` or `AUTHORIZATION_LIBRARY_ACCESS_DENIED`). `NOT_FOUND` → `404`. Everything else → `502` with the message.
Not built: `/auth/local` and `/auth/jellyfin`. Scryer sessions last only 3 days and Wholphin logs in only at startup, so login mode would need bridge-side session state. Add it only if someone can't use API keys.

## Mapping rules
- **Ids (Int):** `int32(fnv1a(s) & 0x7fffffff)` for user and request ids. `DELETE /request/{n}` finds the UUID by rescanning `myMediaRequests(status:PENDING)`. `ponytail:` collision ceiling is fine at homelab scale.
- **Jellyfin id:** the 32-hex `id=` value from `playbackLinks.href`, reformatted as a dashed UUID.
- **Permissions bitmask** for `/auth/me`: `REQUEST` (32, implies movie and TV) if the user can request in the movie *and* series libraries. Otherwise only the ones that apply: `REQUEST_MOVIE` 262144 / `REQUEST_TV` 524288. `AUTO_APPROVE` 128 if auto-approve is allowed. Never `MANAGE_REQUESTS` or `REQUEST_ADVANCED`.
- **Results without a `tmdbId`** are dropped. Wholphin can't address them.
- **Facet for TV (anime routing), stateless:** `metadataSeries.genresAndThemes` contains `"Anime"` → `ANIME`, else `SERIES` (verified: Frieren has it, Breaking Bad doesn't). `/tv/{id}` and `POST /request` already fetch `metadataSeries` (POST needs it for the title), so this costs no extra call. If `metadataSeries` fails, use the library title's facet, then whether the tmdb id is in the `searchMetadataMulti` `anime` list for that name.
- **`mediaInfo.status`:**
  | Scryer | status |
  |---|---|
  | movie with `mediaFiles > 0`; series with `episodesOwned >= episodesTotal > 0` | 5 |
  | series with `episodesOwned > 0` | 4 |
  | in library without files, or own request APPROVED | 3 |
  | own request PENDING | 2 |
  | else / REJECTED / CANCELED | `mediaInfo` omitted (can be requested again) |
- **`mediaInfo.requests`:** only the user's own PENDING/APPROVED requests. `MediaRequest.status` (Seerr): PENDING→1, APPROVED→2, REJECTED→3. CANCELED rows are dropped. `requestedBy.id = hash(me.id)`. `type` = `movie` for MOVIE, `tv` for SERIES **and** ANIME.
- **TV seasons:** `seasons[]` from `metadataSeries`. Per-season `mediaInfo.seasons[]` from `titlesByExternalIds{collections{collectionType collectionIndex episodesOwned episodesTotal}}` (verified: Apothecary Diaries S1 24/24, S3 6/9). `collectionIndex` is a `String!`, so parse it with `strconv.Atoi` and skip it on error. For `SEASON` collections: owned = total > 0 → 5, owned > 0 → 4, else 3 if in the own request's `requestedMonitorSelection.seasonNumbers` (pending → 2). `SPECIALS` (index 0) is ignored. Series-level `episodesOwned/episodesTotal` already excludes specials.
- **Images:** `posterPath`/`backdropPath` = `/images/media/<hash>`. `/imageproxy/tmdb/t/p/{size}/images/media/<hash>` → `302` to `SCRYER_PUBLIC_URL + /images/media/<hash>/` + `w250` (for `w500`) or `w1280` (for `w…_and_h…`). `<hash>` must match `^[0-9a-f]+$`, else `404`, so the redirect can't be pointed elsewhere.
- **Ratings:** take `externalRatings[source].score` (0–100), or `normalized*10` if `score` is null, rounded to Int. Movie `criticsScore = tomatoes`, `audienceScore = popcorn`. TV `criticsScore = tomatoes`. Missing → field omitted. Live examples: Dune tomatoes 36 / popcorn 65; Breaking Bad tomatoes 96.
- **One GraphQL document per Seerr call.** Details = `metadataMovie` + `titlesByExternalIds` + `myMediaRequests` as aliases in one POST. Discovery/search pages = the list query, then one batched `titlesByExternalIds`.
- **`metadataSeries` fails:** fall back to title, poster and year from `searchMetadataMulti` / the library title, with an empty `seasons`. `POST /request` with no title source → `400`.

## Endpoints

### Phase 1: everything Wholphin calls, so no screen hits a 404
| Seerr | Bridge |
|---|---|
| `GET /auth/me` | `me` → `{id, displayName, permissions}`. null → 401 |
| `GET /settings/public` | static `{cacheImages:true, movie4kEnabled:false, series4kEnabled:false}` |
| `GET /search` | `searchMetadataMulti` → `results[]` (`mediaType` movie/tv) |
| `GET /movie/{id}`, `/tv/{id}` | details (above) |
| `GET /movie\|tv/{id}/ratings` | ratings (above) |
| `GET /movie\|tv/{id}/similar`, `/recommendations` | `discoveryNeighbors` |
| `GET /discover/trending`, `/movies`, `/tv`, `/tv/upcoming` | `discoveryItems` with `trending_now`, `popular_movies`, `popular_series`, `upcoming_series`; `offset=(page-1)*20` |
| `GET /discover/movies/upcoming`, `/discover/genreslider/*`, `/watchproviders/*`, `/person/{id}/combined_credits` | empty |
| `POST /request` | `submitMediaRequest` (library by facet, `externalIds:[{source:"tmdb"}]`, seasons → `requestedMonitorSelection.seasonNumbers`) |
| `DELETE /request/{n}` | `cancelMyMediaRequest` |
| `GET /request` | `myMediaRequests(limit: take, offset: skip)` minus CANCELED → `{pageInfo, results[{id, status, type, media:{tmdbId}, requestedBy}]}` |
| `GET /imageproxy/tmdb/...` | 302 |
| anything else | log + 404 |

### Phase 2: optional
- `PUT /request/{id}` → `updateMyMediaRequest` (edit seasons).
- Personalized rows (`for_you`, `top_rated`) via env section mapping.
- Genre filter (Scryer genres are strings, Seerr's are Int ids, so a lookup table is needed).

## Code (Go, stdlib only)
```
main.go        // env, mux, ListenAndServe
scryer.go      // gql(ctx, token, query, vars, &out) + error code → HTTP status
handlers.go    // one func per endpoint
map.go         // Scryer → Seerr structs (only the fields Wholphin reads), ids, status, bitmask, images
map_test.go    // bitmask, status table, id hash, image path, jellyfin id
Dockerfile     // distroless static
```
Env: `SCRYER_URL` (internal OK), `SCRYER_PUBLIC_URL` (TV-reachable, used for the image redirect; defaults to `SCRYER_URL`), `LISTEN_ADDR=:5055`, `BRIDGE_DRY_RUN=1` (POST /request only runs `previewMyRequestDecision` and logs the outcome).

## Test loop
1. ~~Fix the `tv` permissions and choose the auth mode~~ (done: API key).
2. ~~`go run .`, curl each Phase 1 endpoint~~ (done 2026-11-21, dry run). `go run .`, curl each Phase 1 endpoint, and compare against `seerr-api.yml`. Use `previewMyRequestDecision` instead of a real submit until step 3, because `tv` auto-approves and a real request starts a download.
3. In Wholphin (`default` APK), add the bridge URL as a Seerr server: search → open → request → check it in Scryer Requests. Because `tv` auto-approves, a request is never pending: Cancel won't show, and the request **starts a real download**. Pick a small title. To test cancel, temporarily remove `AUTO_APPROVE_REQUESTS` from `tv` (or use a second user without it), request, cancel while pending, then grant it back.
