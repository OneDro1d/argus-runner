// #419 — runs templates/saga-presence.jmx's verify-saga script EXACTLY as extracted from the template
// (the Go test reads it from the .jmx; nothing here re-implements it) with the two objects JMeter
// gives it: `props` and `SampleResult`.
//
// stdin:  {"script": "...", "props": {...}}
// stdout: {"successful": true|false, "message": "...", "data": "..."}

import groovy.json.JsonSlurper
import groovy.json.JsonOutput

def input = new JsonSlurper().parse(System.in)

class FakeResult {
    boolean successful = false
    String responseMessage = ""
    String data = ""
    void setSuccessful(boolean b) { successful = b }
    void setResponseMessage(String m) { responseMessage = m }
    void setResponseData(String d, String cs) { data = d }
}

def props = new Properties()
(input.props ?: [:]).each { k, v -> props.setProperty(k as String, v as String) }
def result = new FakeResult()

def binding = new Binding([props: props, SampleResult: result, log: null, ctx: null, vars: null])
new GroovyShell(binding).evaluate(input.script as String)

println JsonOutput.toJson([successful: result.successful, message: result.responseMessage, data: result.data])
