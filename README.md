# scryer-wholphin-bridge

Lets the [Wholphin](https://github.com/damontecres/Wholphin) Android TV app browse and request movies and series from [Scryer](https://www.scryer.media/). Wholphin speaks the Seerr API. This bridge serves the parts of that API Wholphin uses and translates each call into Scryer GraphQL.

```
Wholphin (TV) --Seerr REST--> bridge --GraphQL--> Scryer
```

## 1. Prepare a Scryer user

Requests must come from an account that is allowed to request. **Scryer never lets an account with *Manage Titles* request**, because managers are expected to add titles directly. So create a dedicated user (e.g. `tv`) rather than using an admin.

For each library you want on the TV (Movie, Series, Anime), give that user:

- **View**
- **Request**
- **Auto-approve requests** (optional; without it, requests wait for approval in Scryer)

Then, logged in as that user, create an API key in the **API keys** panel of their profile settings.

If **Security → "Only users with Manage System Settings can create or use API keys"** is on, a regular user's key won't work. Turn that setting off.

## 2. Run the bridge

Images for `linux/amd64` and `linux/arm64` are published to `ghcr.io/arashm/scryer-wholphin-bridge`. Tags: `latest` (main), `X.Y.Z` / `X.Y` (release tags `vX.Y.Z`), and `sha-<commit>`.

Docker Compose:

```yaml
services:
  scryer-wholphin-bridge:
    image: ghcr.io/arashm/scryer-wholphin-bridge:latest
    restart: unless-stopped
    ports:
      - "5055:5055"
    environment:
      SCRYER_URL: https://scryer.example.com
```

Docker:

```sh
docker run -d --name scryer-wholphin-bridge --restart unless-stopped \
  -p 5055:5055 \
  -e SCRYER_URL=https://scryer.example.com \
  ghcr.io/arashm/scryer-wholphin-bridge:latest
```

Build the image yourself with `docker build -t scryer-wholphin-bridge .`, or run directly with Go:

```sh
SCRYER_URL=https://scryer.example.com go run .
```

| Variable | Default | |
|---|---|---|
| `SCRYER_URL` | required | Scryer base URL as the bridge reaches it (an internal address is fine) |
| `SCRYER_PUBLIC_URL` | `SCRYER_URL` | Scryer URL as the **TV** reaches it. Images are redirected there |
| `LISTEN_ADDR` | `:5055` | |
| `BRIDGE_DRY_RUN` | off | `1` makes requests only preview (logged), so nothing is submitted |

The bridge stores nothing. Each request carries the user's Scryer API key.

## 3. Connect Wholphin

Use the **default** (or app store) build of Wholphin. The Fire TV build has Discover turned off.

1. Settings → **Seerr integration** → add a server.
2. URL: `http://<bridge-host>:5055`, without `/api/v1`.
3. Choose **API Key** and paste the Scryer API key from step 1.

Discover, search, and the Request button should now work. Requests show up in Scryer under Requests.

## Check it works

```sh
curl -H "X-Api-Key: $KEY" http://localhost:5055/api/v1/auth/me
# {"id":...,"username":"tv","permissions":160}
```

`permissions` is a Seerr bitmask: 32 = can request movies and TV, 128 = auto-approve. If it's 0 or missing, the user can't request anywhere. Check the permissions in step 1 (especially that *Manage Titles* is off).

Every call is logged as one line. Unsupported endpoints are logged as `unhandled:` and get 404.

With **Auto-approve** on, a request from the TV starts a real download immediately, and Wholphin won't show Cancel, because only pending requests can be cancelled. Use `BRIDGE_DRY_RUN=1` while trying things out.

## Limitations

- **Play in Jellyfin** only appears when Scryer has a Jellyfin playback link for the title, which is often missing. Such items still show as Available.
- List rows show January 1 of the release year (Scryer lists only carry the year). Detail pages have the real date.
- Search returns a single page (up to 20 movies plus series).
- Empty: upcoming movies (Scryer has no such section), genre rows, streaming providers, cast/crew pages.
- No username/password login, no 4K, no editing of an existing request. See `PLAN.md`.

## Development

```sh
go test ./...
```

`PLAN.md` has the design: how each Seerr field is mapped, verified Scryer facts, and what's left for phase 2.
