FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /scorer .

FROM gcr.io/distroless/static-debian12:nonroot
# FAULT_RATE cozido na imagem só para testar o rollback automático de canary
# (ex.: --build-arg FAULT_RATE=0.5 produz um "candidato mau" deliberado).
ARG FAULT_RATE=0
ENV FAULT_RATE=$FAULT_RATE
COPY --from=build /scorer /scorer
EXPOSE 8080
ENTRYPOINT ["/scorer"]
