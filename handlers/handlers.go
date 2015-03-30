package handlers

import (
	"net/http"

	"github.com/cloudfoundry-incubator/nsync"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/rata"
)

func New(logger lager.Logger, diegoClient receptor.Client, recipebuilder RecipeBuilder) http.Handler {

	desireAppHandler := NewDesireAppHandler(logger, diegoClient, recipebuilder)
	stopAppHandler := NewStopAppHandler(logger, diegoClient)
	killIndexHandler := NewKillIndexHandler(logger, diegoClient)

	actions := rata.Handlers{
		nsync.DesireAppRoute: http.HandlerFunc(desireAppHandler.DesireApp),
		nsync.StopAppRoute:   http.HandlerFunc(stopAppHandler.StopApp),
		nsync.KillIndexRoute: http.HandlerFunc(killIndexHandler.KillIndex),
	}

	handler, err := rata.NewRouter(nsync.Routes, actions)
	if err != nil {
		panic("unable to create router: " + err.Error())
	}

	return handler
}
