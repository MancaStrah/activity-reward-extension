package fccutils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/flare-foundation/go-flare-common/pkg/logger"
	csigning "github.com/flare-foundation/go-flare-common/pkg/signing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/flare-foundation/go-flare-common/pkg/tee/attestation/googlecloud"
	"github.com/flare-foundation/tee-node/pkg/attestation"
	"github.com/flare-foundation/tee-node/pkg/types"
	"github.com/pkg/errors"
)

const repeats = 15

// maxResponseBytes caps a proxy/node response body decoded into memory, so a
// misbehaving or hostile endpoint cannot stream an unbounded body into a tool.
const maxResponseBytes = 8 << 20 // 8 MiB

// httpClient is shared and carries a timeout. The bare http.Get/http.Post default
// client has NO timeout, so a stalled proxy or node would hang a tool indefinitely.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// pollClient is for the result-polling loop only: a poll iteration is cheap and
// retried anyway, so a short per-request timeout keeps the worst-case loop
// duration bounded: repeats × (timeout + sleep) ≈ 1¾ min, where the 30 s timeout
// used elsewhere in this file would allow roughly ~8.
var pollClient = &http.Client{Timeout: 5 * time.Second}

func TeeInfo(nodeURL string) (*types.SignedTeeInfoResponse, error) {
	result, err := httpClient.Get(nodeURL + "/info")
	if err != nil {
		return nil, errors.Errorf("%s", err)
	}
	defer result.Body.Close()

	// A non-200 body (error page, proxy HTML) must not be JSON-decoded as if it
	// were TEE info — surface the status instead.
	if result.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(result.Body, 512))
		return nil, errors.Errorf("GET %s/info returned %d: %s", nodeURL, result.StatusCode, strings.TrimSpace(string(body)))
	}

	var teeInfo types.SignedTeeInfoResponse
	err = json.NewDecoder(io.LimitReader(result.Body, maxResponseBytes)).Decode(&teeInfo)
	if err != nil {
		return nil, errors.Errorf("%s", err)
	}

	return &teeInfo, nil
}

func CodeHashAndPlatform(attestationString string) (common.Hash, common.Hash, error) {
	claims := attestation.NeededClaims{}
	_, _, err := googlecloud.ParsePKITokenUnverifiedClaims(attestationString, &claims)
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("%s", err)
	}

	codeHash, err := claims.CodeHash()
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("%s", err)
	}
	platform, err := claims.Platform()
	if err != nil {
		return common.Hash{}, common.Hash{}, errors.Errorf("%s", err)
	}

	return codeHash, platform, nil
}

func TeeProxyId(teeInfo *types.SignedTeeInfoResponse) (common.Address, common.Address, error) {
	pubKey, err := types.ParsePubKey(teeInfo.TeeInfo.PublicKey)
	if err != nil {
		return common.Address{}, common.Address{}, errors.Errorf("%s", err)
	}

	teeID := crypto.PubkeyToAddress(*pubKey)

	hash, err := teeInfo.TeeInfo.Hash()
	if err != nil {
		return common.Address{}, common.Address{}, errors.Errorf("%s", err)
	}
	// The proxy signs the TEE info over a domain-separated, chain-ID-bound
	// payload (Payload{ProxyTeeInfo, chainID, infoHash}) — see tee-proxy
	// external.go. Recover the proxy address over the SAME preimage, or the
	// proxyId comes out garbage and the on-chain availability check is rejected
	// by the verifier with "proxy signer does not match".
	infoSignHash, err := csigning.NewPayload(csigning.ProxyTeeInfo, teeInfo.TeeInfo.ChainID, common.BytesToHash(hash)).Hash()
	if err != nil {
		return common.Address{}, common.Address{}, errors.Errorf("%s", err)
	}
	proxyPubKey, err := crypto.SigToPub(accounts.TextHash(infoSignHash[:]), teeInfo.ProxySignature)
	if err != nil {
		return common.Address{}, common.Address{}, errors.Errorf("%s", err)
	}
	proxyID := crypto.PubkeyToAddress(*proxyPubKey)

	return teeID, proxyID, nil
}

func ActionResult(nodeURL string, actionID common.Hash) (*types.ActionResponse, error) {
	url := nodeURL + "/action/result/" + actionID.Hex()
	var lastErr error
	for i := range repeats {
		resp, err := pollClient.Get(url)
		if err != nil {
			lastErr = errors.Errorf("%s", err)
		} else if resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var response types.ActionResponse
			if decErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&response); decErr != nil {
				return nil, errors.Errorf("%s", decErr)
			}
			return &response, nil
		} else {
			// Close the body of a non-OK response before the next attempt overwrites
			// the reference — otherwise each retry leaks a connection.
			resp.Body.Close()
			logger.Warnf("action result status not ok: got: %d for %s, %s", resp.StatusCode, actionID.Hex(), nodeURL)
			lastErr = errors.Errorf("action result status not ok, got: %d", resp.StatusCode)
		}
		if i < repeats-1 {
			time.Sleep(2 * time.Second)
		}
	}
	return nil, lastErr
}

func SetProxyUrl(configurationPort int, proxyPort int) error {
	url := fmt.Sprintf("http://localhost:%d", proxyPort)
	request := types.ConfigureProxyURLRequest{
		URL: &url,
	}

	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	url = fmt.Sprintf("http://localhost:%d/proxy", configurationPort)
	logger.Infof("Setting proxy url on tee: %s", url)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
