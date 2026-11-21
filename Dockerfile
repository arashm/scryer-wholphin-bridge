FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /bridge .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /bridge /bridge
EXPOSE 5055
ENTRYPOINT ["/bridge"]
