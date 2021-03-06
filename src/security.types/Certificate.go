package	sectypes

import (
	"../types"
	"io"
	"net"
	"bytes"
	"crypto"
	"strconv"
	"net/url"
	"net/http"
	"io/ioutil"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"encoding/base64"
	"golang.org/x/crypto/ocsp"
)


const	MIN_STAPLE_SIZE	= 5
var	issuers map[string]*x509.Certificate = make(map[string]*x509.Certificate,100)

var 	issuerMutex	sync.Mutex

type	Cert	struct {
	file	types.Path
	cert	tls.Certificate
	invalid	bool
}

func (cp *Cert) UnmarshalTOML(data []byte) error  {
	return (&cp.file).UnmarshalTOML(data)
}



func (cp *Cert) Certificate() tls.Certificate {
	return	cp.cert
}


func (cp *Cert) CommonName() string {
	return	cp.cert.Leaf.Subject.CommonName
}


func (cp *Cert) IsEnabledFor(zone string) bool {
	return	cp.cert.Leaf.VerifyHostname(zone) == nil
}


func (cp *Cert) IsEnabled() bool {
	if cp.file == "" || cp.invalid {
		return false
	}

	if len(cp.cert.Certificate) > 0{
		return true
	}

	var err error
	crt,err		:= file2pem(cp.file.String())
	if err != nil {
		cp.invalid = true
		return false
	}

	cp.cert.Leaf,err		= x509.ParseCertificate(crt.Bytes)
	if err != nil {
		cp.invalid = true
		return false
	}

	cp.LoadChain()
	cp.RefreshOCSP()

	return true
}


func (cp *Cert)LoadChain() {
	stack	:= make([]*x509.Certificate,0,10)
	cp.cert.Certificate	= append(cp.cert.Certificate, cp.cert.Leaf.Raw)
	stack			= append(stack, cp.cert.Leaf)
	for len(stack)>0 {
		cert	:= stack[0]
		stack	=  stack[1:]
		for _,issuing := range cert.IssuingCertificateURL {
			issuer, err := load_issuer(issuing)
			if err == nil {
				cp.cert.Certificate = append(cp.cert.Certificate, issuer.Raw)
				stack = append(stack, issuer)
			}
		}
	}
}



func (cp *Cert)RefreshOCSP() (err error) {
	if cp.IsEnabled() && len(cp.cert.Certificate)>1 {
		for _,ocsp_server := range cp.cert.Leaf.OCSPServer {
			for _,issuing := range cp.cert.Leaf.IssuingCertificateURL {
				issuer, err := load_issuer(issuing)
				if err != nil {
					return err
				}

				request,err := ocsp.CreateRequest(cp.cert.Leaf, issuer, &ocsp.RequestOptions { crypto.SHA1 })
				if err !=nil {
					return err
				}

				staple := get_or_post_OCSP(ocsp_server,"application/ocsp-request",request)
				if len(staple) <MIN_STAPLE_SIZE {
					return nil
				}

				resp,err := ocsp.ParseResponse(staple, issuer )
				//log.Printf("\n%+v\n", struct{
				//		ProducedAt, ThisUpdate, NextUpdate string
				//	}{ resp.ProducedAt.Format(time.RFC3339), resp.ThisUpdate.Format(time.RFC3339), resp.NextUpdate.Format(time.RFC3339) } )
				if err != nil {
					continue
				}

				switch resp.Status {
					case ocsp.Good, ocsp.Revoked:
						cp.cert.OCSPStaple = staple
						return nil

					case ocsp.Unknown, ocsp.ServerFailed:
				}
			}
		}
	}
	return err
}


func load_issuer(issuing string) (*x509.Certificate, error) {
	issuerMutex.Lock()
	defer issuerMutex.Unlock()

	if issuer, ok := issuers[issuing]; ok {
		return issuer,nil
	}

	resp, err	:= http.Get(issuing)
	if err!= nil {
		return nil,err
	}
	defer	resp.Body.Close()

	issuer,err	:= ioutil.ReadAll(resp.Body)
	if err!= nil {
		return nil,err
	}

	cert,err	:= x509.ParseCertificate(issuer)
	if err!= nil {
		return nil,err
	}

	issuers[issuing] = cert
	return cert, nil
}


func needs_panic(err error) bool {
	if err == nil	{
		return false
	}

	if err == io.EOF {
		return false
	}

	switch err.(type) {
		case net.Error:
			t_err := err.(net.Error)
			return !(t_err.Timeout() || t_err.Temporary())

		case *url.Error:
			t_err := err.(*url.Error).Err.(net.Error)
			return !(t_err.Timeout() || t_err.Temporary())
	}

	return true
}


func get_or_post_OCSP(url string, mime string, data []byte) []byte {
	var	err	error
	var	rsp	*http.Response

	get_url := url + "/"+base64.URLEncoding.EncodeToString(data)

	if len(get_url)<255 {
		rsp,err	= http.Get(get_url)

		if err!= nil {
			if needs_panic(err) {
				panic(err)
			}
			return []byte{}
		}
		//log.Printf("\n-G----\n%s\n%+v %+v\n----\n\n", get_url, rsp.Status, rsp.Header)
		defer rsp.Body.Close()
	}

	need_post	:= false
	switch {
		case len(get_url)>=255:
			need_post	= true

		case err != nil:
			need_post	= true

		default:
			v,_ := strconv.Atoi(rsp.Header.Get("Content-Length"))
			need_post	= v <= MIN_STAPLE_SIZE
	}

	if need_post {
		rsp,err	= http.Post(url, mime, bytes.NewReader(data))
		//log.Printf("\n-P----\n%+v %+v\n----\n\n", rsp.Status, rsp.Header)
		if err!= nil {
			if needs_panic(err) {
				panic(err)
			}
			return []byte{}
		}
		defer rsp.Body.Close()
	}

	body,err:= ioutil.ReadAll(rsp.Body)
	if err!= nil {
		if needs_panic(err) {
			panic(err)
		}
		return []byte{}
	}
	return body
}
