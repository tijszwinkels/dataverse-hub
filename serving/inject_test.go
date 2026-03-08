package serving

import "testing"

func TestInjectBaseDomain(t *testing.T) {
	tests := []struct {
		name       string
		html       string
		baseDomain string
		want       string
	}{
		{
			name:       "empty baseDomain returns unchanged",
			html:       "<html><head><title>Hi</title></head></html>",
			baseDomain: "",
			want:       "<html><head><title>Hi</title></head></html>",
		},
		{
			name:       "injects after <head>",
			html:       "<html><head><title>Hi</title></head></html>",
			baseDomain: "dataverse001.net",
			want:       `<html><head>` + "\n" + `<meta name="dv-base-domain" content="dataverse001.net"><title>Hi</title></head></html>`,
		},
		{
			name:       "injects after <head> with attributes",
			html:       `<html><head lang="en"><title>Hi</title></head></html>`,
			baseDomain: "example.com",
			want:       `<html><head lang="en">` + "\n" + `<meta name="dv-base-domain" content="example.com"><title>Hi</title></head></html>`,
		},
		{
			name:       "case insensitive HEAD",
			html:       "<HTML><HEAD><TITLE>Hi</TITLE></HEAD></HTML>",
			baseDomain: "example.com",
			want:       `<HTML><HEAD>` + "\n" + `<meta name="dv-base-domain" content="example.com"><TITLE>Hi</TITLE></HEAD></HTML>`,
		},
		{
			name:       "no head tag prepends",
			html:       "<div>Hello</div>",
			baseDomain: "example.com",
			want:       `<meta name="dv-base-domain" content="example.com">` + "\n" + "<div>Hello</div>",
		},
		{
			name:       "real page with doctype",
			html:       "<!DOCTYPE html>\n<html><head>\n<meta charset=\"utf-8\">\n</head><body></body></html>",
			baseDomain: "dataverse001.net",
			want:       "<!DOCTYPE html>\n<html><head>\n<meta name=\"dv-base-domain\" content=\"dataverse001.net\">\n<meta charset=\"utf-8\">\n</head><body></body></html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectBaseDomain(tt.html, tt.baseDomain)
			if got != tt.want {
				t.Errorf("injectBaseDomain()\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}
