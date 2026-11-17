// Command hosted-keygen prints a new Ed25519 key pair in the PEM-with-kid
// format internal/hostedauth reads: the private key block, then the public one.
// With -out PREFIX it writes PREFIX.key.pem (0600) and PREFIX.pub.pem instead.
package main

import (
	"flag"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hostedauth"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, PartsExclude: []string{zerolog.TimestampFieldName}})
	kid := flag.String("kid", "", "key id stamped into every token this key signs, e.g. api-2026-11 (required)")
	out := flag.String("out", "", "write PREFIX.key.pem and PREFIX.pub.pem instead of printing")
	flag.Parse()
	if *kid == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	key, err := hostedauth.GenerateSigningKey(*kid)
	if err != nil {
		log.Fatal().Err(err).Msg("generate key")
	}
	priv, err := key.MarshalPEM()
	if err != nil {
		log.Fatal().Err(err).Msg("encode private key")
	}
	pub, err := key.MarshalPublicPEM()
	if err != nil {
		log.Fatal().Err(err).Msg("encode public key")
	}

	if *out == "" {
		if _, err := os.Stdout.Write(append(priv, pub...)); err != nil {
			log.Fatal().Err(err).Msg("write stdout")
		}
		return
	}
	if err := os.WriteFile(*out+".key.pem", priv, 0o600); err != nil {
		log.Fatal().Err(err).Msg("write private key")
	}
	if err := os.WriteFile(*out+".pub.pem", pub, 0o644); err != nil {
		log.Fatal().Err(err).Msg("write public key")
	}
	log.Info().Str("kid", *kid).Str("private", *out+".key.pem").Str("public", *out+".pub.pem").Msg("wrote key pair")
}
