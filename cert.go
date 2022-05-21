package main

import (
	"crypto/tls"
	"crypto/x509"
	"embed"
	"fmt"
	"os"
)

var (
	//go:embed tokens/*
	embeddedFile embed.FS
)

func getServerName(rawCert []byte) (string, error) {
	cert, err := x509.ParseCertificate(rawCert)
	if err != nil {
		return "", err
	}
	return cert.Subject.CommonName, nil
}

func getTLSConfigFromEmbeded() (*tls.Config, error) {
	caPEM, err := embeddedFile.ReadFile("tokens/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded ca.crt: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA format")
	}
	crtPEM, err := embeddedFile.ReadFile("tokens/cert.crt")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded cert.crt: %v", err)
	}
	keyPEM, err := embeddedFile.ReadFile("tokens/cert.key")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded cert.key: %v", err)
	}
	cert, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %v", err)
	}
	return &tls.Config{
		RootCAs:      caPool,
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"clover3"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func getTLSConfigFromEnv() (*tls.Config, error) {
	if len(os.Getenv("CLOVER_CA")) == 0 {
		return nil, fmt.Errorf("`CLOVER_CA` is undefined")
	}
	if len(os.Getenv("CLOVER_CRT")) == 0 {
		return nil, fmt.Errorf("`CLOVER_CRT` is undefined")
	}
	if len(os.Getenv("CLOVER_KEY")) == 0 {
		return nil, fmt.Errorf("`CLOVER_KEY` is undefined")
	}
	caPEM, err := os.ReadFile(os.Getenv("CLOVER_CA"))
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA format")
	}
	cert, err := tls.LoadX509KeyPair(os.Getenv("CLOVER_CRT"), os.Getenv("CLOVER_KEY"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		RootCAs:      caPool,
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"clover3"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}
