FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /platform-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /platform-api /platform-api
EXPOSE 8090
ENTRYPOINT ["/platform-api"]
