# EVA Remote Repository Runbook

> 작성 중

이 문서는 `repository.mode: remote` 환경의 전체 설치 절차를 제공할 예정입니다.

Remote Target은 외부 Registry, AWS, Hugging Face 및 외부 Helm Repository에 직접 접근하지 않습니다. Main Harbor와 Main 서버가 준비한 설치 자산만 사용합니다.

Main 준비 완료 확인, Workspace 작성, Target 설치, 공급 경로 검증, 재실행 및 장애 조치를 이 문서 하나에 포함합니다.

구현 설계는 ../architecture/remote-mode-implementation-design.md을 참고합니다.

Main 서버에서는 Target 전달 전에 준비 결과를 확인합니다.

```bash
sudo eva remote prepare . --registry <main-harbor>
sudo eva remote verify . --registry <main-harbor>
sudo eva remote publish . --target <user@target>
```

이 검증은 local preparation evidence를 대상으로 하며, Main Harbor live availability는 E2E에서 확인합니다.
