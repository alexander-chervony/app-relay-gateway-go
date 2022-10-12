// Copyright (c) 2022 Cloudflare, Inc. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/chris-wood/ohttp-go"
)

type gatewayResource struct {
	verbose               bool
	keyID                 uint8
	gateway               ohttp.Gateway
	encapsulationHandlers map[string]EncapsulationHandler
	debug                 bool
	metricsFactory        MetricsFactory
}

const (
	ohttpRequestContentType  = "message/ohttp-req"
	ohttpResponseContentType = "message/ohttp-res"
	twelveHours              = 12 * 3600
	twentyFourHours          = 24 * 3600

	// Metrics constants
	metricsEventMarshalRequest      = "marshal_request"
	metricsEventGatewayRequest      = "gateway_request"
	metricsResultInvalidMethod      = "invalid_method"
	metricsResultInvalidContentType = "invalid_content_type"
	metricsResultInvalidContent     = "invalid_content"
)

func (s *gatewayResource) httpError(w http.ResponseWriter, status int, debugMessage string) {
	if s.verbose {
		log.Println(debugMessage)
	}
	if s.debug {
		http.Error(w, debugMessage, status)
		w.Write([]byte(debugMessage))
	} else {
		http.Error(w, http.StatusText(status), status)
	}
}

func (s *gatewayResource) gatewayHandler(w http.ResponseWriter, r *http.Request) {
	if s.verbose {
		log.Printf("%s Handling %s\n", r.Method, r.URL.Path)
	}

	metrics := s.metricsFactory.Create(metricsEventGatewayRequest)

	if r.Method != http.MethodPost {
		metrics.Fire(metricsResultInvalidMethod)
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Invalid method: %s", r.Method))
		return
	}
	if r.Header.Get("Content-Type") != ohttpRequestContentType {
		metrics.Fire(metricsResultInvalidContentType)
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Invalid content type: %s", r.Header.Get("Content-Type")))
		return
	}

	var encapHandler EncapsulationHandler
	var ok bool
	if encapHandler, ok = s.encapsulationHandlers[r.URL.Path]; !ok {
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Unknown handler for %s", r.URL.Path))
		return
	}

	defer r.Body.Close()
	encryptedMessageBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		metrics.Fire(metricsResultInvalidContent)
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Reading request body failed: %s", err))
		return
	}

	if s.verbose {
		// todo: do we really need it at this point?
		log.Printf("Request body: %s\n", hex.EncodeToString(encryptedMessageBytes))
	}

	encapsulatedReq, err := ohttp.UnmarshalEncapsulatedRequest(encryptedMessageBytes)
	if err != nil {
		metrics.Fire(metricsResultInvalidContent)
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Reading request body failed"))
		return
	}

	encapsulatedResp, err := encapHandler.Handle(r, encapsulatedReq, metrics)
	if err != nil {
		if s.verbose {
			log.Println(err)
		}
		if err == ConfigMismatchError {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		} else if err == GatewayTargetForbiddenError {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		} else {

			// todo: (here and earlier occurences up)
			// call s.httpError to have everything logged properly?

			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
	}

	packedResponse := encapsulatedResp.Marshal()

	w.Header().Set("Content-Type", ohttpResponseContentType)
	w.Header().Set("Connection", "Keep-Alive")
	w.Write(packedResponse)
}

func (s *gatewayResource) marshalHandler(w http.ResponseWriter, r *http.Request) {
	if !s.debug {
		s.httpError(w, http.StatusForbidden, "Forbidden. Allowed in debug mode only.")
	}

	if s.verbose {
		log.Printf("%s Handling %s\n", r.Method, r.URL.Path)
	}

	metrics := s.metricsFactory.Create(metricsEventMarshalRequest)
	metrics.Fire(metricsResultRequested)

	if r.Method != http.MethodPost {
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Invalid method: %s", r.Method))
		return
	}

	var encapHandler EncapsulationHandler
	var ok bool
	if encapHandler, ok = s.encapsulationHandlers[r.URL.Path]; !ok {
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Unknown handler for %s", r.URL.Path))
		return
	}

	packedRequest, err := encapHandler.Handle(r, ohttp.EncapsulatedRequest{}, metrics)
	if err != nil {
		s.httpError(w, http.StatusBadRequest, fmt.Sprintf("Encapsulation failed: %s", err))
		return
	}

	s.httpError(w, http.StatusInternalServerError, "Config unavailable")
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

	content := packedRequest.Marshal()
	w.Header().Set("Content-Type", ohttpResponseContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Write(content)

	metrics.Fire(metricsResultSuccess)
}

func (s *gatewayResource) configHandler(w http.ResponseWriter, r *http.Request) {
	if s.verbose {
		log.Printf("%s Handling %s\n", r.Method, r.URL.Path)
	}

	config, err := s.gateway.Config(s.keyID)
	if err != nil {
		log.Printf("Config unavailable")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	// Make expiration time even/random throughout interval 12-36h
	rand.Seed(time.Now().UnixNano())
	maxAge := twelveHours + rand.Intn(twentyFourHours)
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, private", maxAge))

	w.Write(config.Marshal())
}
