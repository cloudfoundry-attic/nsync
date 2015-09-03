package handlers

import (
	"net/http"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/nsync"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/rata"
)

func New(logger lager.Logger, bbsClient bbs.Client, recipebuilders map[string]recipebuilder.RecipeBuilder) http.Handler {

	desireAppHandler := NewDesireAppHandler(logger, bbsClient, recipebuilders)
	stopAppHandler := NewStopAppHandler(logger, bbsClient)
	killIndexHandler := NewKillIndexHandler(logger, bbsClient)

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
