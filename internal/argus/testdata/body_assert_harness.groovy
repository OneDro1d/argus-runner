// — runs the "enforce body assertion (DF-04)" JSR223 assertion of a template EXACTLY as
// extracted from the .jmx (the Go test reads it from the file; nothing here re-implements it) with the
// objects JMeter gives it: `props`, `prev` and `AssertionResult`.
//
// stdin:  {"script": "...", "props": {...}, "body": "...", "code": "200"}
// stdout: {"failure": true|false, "message": "..."}

import groovy.json.JsonSlurper
import groovy.json.JsonOutput

def input = new JsonSlurper().parse(System.in)

class FakePrev {
    String body = ""
    String code = "200"
    String getResponseDataAsString() { body }
    String getResponseCode() { code }
    String getResponseHeaders() { "" }
}
class FakeAssertion {
    boolean failure = false
    String message = ""
    void setFailure(boolean b) { failure = b }
    void setFailureMessage(String m) { message = m }
}

def props = new Properties()
(input.props ?: [:]).each { k, v -> props.setProperty(k as String, v as String) }
def prev = new FakePrev(body: (input.body ?: "") as String, code: (input.code ?: "200") as String)
def ar = new FakeAssertion()

def binding = new Binding([props: props, prev: prev, AssertionResult: ar])
new GroovyShell(binding).evaluate(input.script as String)

println JsonOutput.toJson([failure: ar.failure, message: ar.message])
