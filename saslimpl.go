//go:build sasl
// +build sasl

package mgo

import (
	"github.com/CardInfoLink/mgo/internal/sasl"
)

func saslNew(cred Credential, host string) (saslStepper, error) {
	return sasl.New(cred.Username, cred.Password, cred.Mechanism, cred.Service, host)
}
