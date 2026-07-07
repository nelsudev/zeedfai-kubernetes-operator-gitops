package controllers

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Métricas exportadas pelo próprio operator (não pelo scorer), para que o
// platform-api/GUI (Fase 6) tenha uma série temporal de lag/réplicas mesmo
// quando não há eventos suficientes vindos do scorer.
var (
	consumerLagGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zeedfai_operator_consumer_lag",
		Help: "Consumer lag observado pelo operator na última avaliação de autoscaling.",
	}, []string{"pipeline"})

	desiredReplicasGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zeedfai_operator_desired_replicas",
		Help: "Réplicas decididas pelo autoscaler para o pipeline.",
	}, []string{"pipeline"})

	readyReplicasGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "zeedfai_operator_ready_replicas",
		Help: "Réplicas prontas do Deployment do scorer.",
	}, []string{"pipeline"})
)
