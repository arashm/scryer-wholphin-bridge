FROM --platform=$BUILDPLATFORM golang:1.23 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /bridge .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /bridge /bridge
EXPOSE 5055
ENTRYPOINT ["/bridge"]
