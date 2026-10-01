package webapi

import "testing"

func TestParseRoot(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><root><status>ok</status><pwState>1</pwState><sysDesc>PowerEdge R710</sysDesc><sensor><name>Temp</name><val>21</val></sensor></root>`)
	m, err := parseRoot(body)
	if err != nil {
		t.Fatal(err)
	}
	if m["status"] != "ok" || m["pwState"] != "1" || m["sysDesc"] != "PowerEdge R710" {
		t.Fatalf("got %v", m)
	}
	if m["sensor"] != "<name>Temp</name><val>21</val>" {
		t.Fatalf("nested: %q", m["sensor"])
	}
}

func TestParseJNLP(t *testing.T) {
	body := []byte(`<jnlp codebase="https://192.168.11.221:443/"><application-desc main-class="com.avocent.idrac.kvm.Main">
<argument>ip=192.168.11.221</argument><argument>kmport=5900</argument><argument>vport=5900</argument>
<argument>user=472983611</argument><argument>passwd=1839471623</argument><argument>apcp=1</argument><argument>version=2</argument><argument>vmprivilege=true</argument><argument>title=idrac-go</argument>
</application-desc></jnlp>`)
	l, err := ParseJNLP(body)
	if err != nil {
		t.Fatal(err)
	}
	if l.Host != "192.168.11.221" || l.KMPort != 5900 || l.User != "472983611" || l.Password != "1839471623" || !l.APCP || !l.VMPrivilege {
		t.Fatalf("got %+v", l)
	}
}

func TestTokenRegexp(t *testing.T) {
	m := tokenRe.FindAllStringSubmatch("index.html?ST1=abc123DEF&ST2=zz99", -1)
	if len(m) != 2 || m[0][2] != "abc123DEF" || m[1][2] != "zz99" {
		t.Fatalf("got %v", m)
	}
}

func TestEncodeSetValue(t *testing.T) {
	if got := encodeSetValue("a:b,c d"); got != "a%3Ab%2Cc%20d" {
		t.Fatalf("got %q", got)
	}
}

func TestParseList(t *testing.T) {
	items := ParseList(`<eventLogEntry><severity>Critical</severity><dateTime>Sat Sep 26 2026 01:02:03</dateTime><description>DIMM_B8 error</description></eventLogEntry><eventLogEntry id="2"><severity>Ok</severity></eventLogEntry>`)
	if len(items) != 2 || items[0]["severity"] != "Critical" || items[0]["description"] != "DIMM_B8 error" || items[1]["id"] != "2" {
		t.Fatalf("got %v", items)
	}
}

func TestParseListWrapped(t *testing.T) {
	items := ParseList(`<thresholdSensorList><sensor><name>Ambient Temp</name><reading>21</reading><sensorStatus>2</sensorStatus></sensor><sensor><name>CPU1</name><reading>40</reading></sensor></thresholdSensorList>`)
	if len(items) != 2 || items[0]["name"] != "Ambient Temp" || items[1]["reading"] != "40" {
		t.Fatalf("wrapped: %v", items)
	}
	one := ParseList(`<thresholdSensorList><sensor><name>Only</name><reading>1</reading></sensor></thresholdSensorList>`)
	if len(one) != 1 || one[0]["name"] != "Only" {
		t.Fatalf("single wrapped: %v", one)
	}
	if ParseList("") != nil && len(ParseList("")) != 0 {
		t.Fatal("empty")
	}
}
