import axios from "axios";

// Real defect: this is an OUTBOUND client to a third party (Allpass), not a route of the SUT —
// found for real in packages/kyc-allpass/src/clients/allpass.ts.
export class AllpassClient {
  private api: any;

  constructor() {
    this.api = axios.create({ baseURL: "https://api.allpass.example" });
  }

  async verify(body: unknown) {
    return this.api.post("/verification", body);
  }

  async status(applicantId: string) {
    return this.api.get(`/applicant/${applicantId}/status`);
  }
}
