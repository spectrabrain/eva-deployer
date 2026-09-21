# EVA Remote Asset Preparation

> 작성 중

이 문서는 Remote Repository 설치를 위해 Main 서버에서 수행하는 자산 준비 절차를 제공할 예정입니다.

대상 범위:

- Release 및 version 검증
- Container image 다운로드와 Main Harbor publish
- EVA Agent 및 vLLM model cache 준비
- Qdrant snapshot OCI artifact publish
- Runtime 및 OS package payload 준비
- 준비 결과 manifest와 무결성 검증

Target 설치 절차는 [Remote Repository Runbook](../installation/remote-repository-runbook.md)을 사용합니다.

정상 준비 흐름은 다음과 같습니다.

```bash
sudo eva remote prepare . --registry <main-harbor>
sudo eva remote verify . --registry <main-harbor>
sudo eva remote publish . --target <user@target>
```

`remote verify`는 준비 디렉터리의 local evidence만 읽어 검증합니다. Main Harbor의 현재 가용성은 별도 E2E 절차에서 확인합니다.
