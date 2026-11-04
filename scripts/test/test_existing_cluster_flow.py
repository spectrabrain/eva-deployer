#!/usr/bin/env python3
"""Exercise shared Ansible preconditions/dependencies against an isolated NKS fixture."""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
ANSIBLE = shutil.which("ansible-playbook")
NEW = "211722900905.dkr.ecr.ap-northeast-2.amazonaws.com/spectrabrain/"
OLD = "339713051385.dkr.ecr.ap-northeast-2.amazonaws.com/mellerikat/release/"

FAKE = r'''
import base64,json,os,pathlib,sys
root = pathlib.Path(os.environ['CLUSTER_FIXTURE'])
state = json.loads((root/'state.json').read_text())
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with (root/'calls.jsonl').open('a') as f:
 f.write(json.dumps({'tool':tool,'args':args,'kubeconfig':os.environ.get('KUBECONFIG'),
                    'incoming':os.environ.get('AWS_ACCESS_KEY_ID')=='AKIAfixtureIncoming'})+'\n')
def output(value): print(json.dumps(value)); sys.exit(0)
def ns():
 for flag in ['-n','--namespace']:
  if flag in args: return args[args.index(flag)+1]
 return 'eva-agent'
if tool == 'aws':
 if os.environ.get('AWS_ACCESS_KEY_ID') != 'AKIAfixtureIncoming': sys.exit(1)
 if 'get-login-password' in args: print('fixture-token-DO-NOT-PRINT'); sys.exit(0)
 output({'images':[{'imageId':{'imageTag':'3.3.0'}}], 'failures':[]})
if tool == 'kustomize': print('v5.4.3'); sys.exit(0)
if tool == 'helm':
 namespace = ns(); component = namespace.removeprefix('eva-'); release=namespace
 if args[0]=='list':
  if state.get('new_install'): output([])
  if '--filter' in args: release=args[args.index('--filter')+1].strip('^$').replace(chr(92),'')
  output([{'name':release,'namespace':namespace,'revision':'1','chart':release+'-3.2.1','app_version':'3.2.1','status':'deployed'}])
 if args[:2]==['get','values']: output(state['values'].get(component,{}))
 if args[:2]==['get','manifest']: print(''); sys.exit(0)
 if args[0]=='status': output({'info':{'status':'deployed'}})
 if args[:2]==['upgrade','--install']: print('fixture deployed'); sys.exit(0)
 if args[0]=='template': output({'kind':'Deployment','metadata':{'name':args[1],'namespace':'eva-agent'},
   'spec':{'replicas':1,'template':{'spec':{'serviceAccountName':'sa-eva-agent','containers':[]}}}})
 raise SystemExit('Unexpected Helm mutation')
if tool != 'kubectl': raise SystemExit('Unexpected host command')
if args[:2]==['config','current-context']: print('nks-fixture'); sys.exit(0)
if 'get' in args:
 kind=args[args.index('get')+1]
 if kind=='nodes': output({'items':state['nodes']})
 if kind=='storageclass': print('storageclass/'+args[args.index('get')+2]); sys.exit(0)
 if kind=='storageclasses': output({'items':[{'metadata':{'name':'nks-block','annotations':{'storageclass.kubernetes.io/is-default-class':'true'}}},{'metadata':{'name':'nks-nas'}}]})
 if kind in ['runtimeclasses','ingressclasses','jobs']: output({'items':[]})
 if kind=='deployments': output({'items':state['deployments'].get(ns(),[])})
 if kind=='deployments,statefulsets': output({'items':state['deployments'].get(ns(),[])})
 if kind=='pv': output(state['vllm_pv'])
 if kind=='pvc': output(state['vllm_pvc'])
 if kind=='serviceaccount': sys.exit(0 if state.get('agent_sa') else 1)
 if kind=='serviceaccounts': output({'items':[{'kind':'ServiceAccount','metadata':{'name':'sa-eva-agent','namespace':'eva-agent'}}] if state.get('agent_sa') else []})
 if kind=='pods': output({'items':state['pods'].get(ns(),[])})
 if kind=='endpointslices': output({'items':[{'endpoints':[{'addresses':['198.18.1.1'],'conditions':{'ready':True}}]}]})
 if kind=='secret':
  name=args[args.index('get')+2]
  if name!='aws-credentials':
   if state.get('pull_secret'): output(state['pull_secret'])
   print(''); sys.exit(0)
  secret=dict(state['secret']); secret['metadata']=dict(secret['metadata'],namespace=ns()); output(secret)
 if kind=='cronjob': print(''); sys.exit(0)
 if ',' in kind: print(''); sys.exit(0)
 output({'items':[]})
if 'create' in args and '--dry-run=client' in args:
 if 'namespace' in args: output({'apiVersion':'v1','kind':'Namespace','metadata':{'name':args[args.index('namespace')+1]}})
 output({'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':'sa-eva-agent','namespace':ns()}})
if 'create' in args and 'serviceaccount' in args:
 state['agent_sa']=True; (root/'state.json').write_text(json.dumps(state)); print('fixture created'); sys.exit(0)
if 'create' in args and '-f' in args:
 value=json.load(sys.stdin); value['metadata'].update(uid='pull-uid',resourceVersion='1')
 state['pull_secret']=value; (root/'state.json').write_text(json.dumps(state))
 print('fixture created'); sys.exit(0)
if 'apply' in args:
 if args[-1]!='-': state['renewal']=pathlib.Path(args[-1]).read_text(); (root/'state.json').write_text(json.dumps(state))
 else: sys.stdin.read()
 print('fixture created'); sys.exit(0)
if 'patch' in args:
 fields=json.load(sys.stdin)
 state['secret']['data'].update(fields.get('data',{}))
 state['secret']['metadata'].setdefault('annotations',{}).update(fields.get('metadata',{}).get('annotations',{}))
 state['secret']['metadata']['resourceVersion']='2'
 (root/'state.json').write_text(json.dumps(state))
 print('fixture patched'); sys.exit(0)
raise SystemExit('Unexpected kubectl command')
'''


@unittest.skipUnless(ANSIBLE, "ansible-playbook required")
class SharedFlowTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="eva-nks-flow-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.release = self.root / "release"
        self.release.mkdir()
        (self.release / "src").symlink_to(ROOT / "src", target_is_directory=True)
        self.workspace = self.root / "workspace"
        for d in ["site-values", "credentials"]:
            (self.workspace / d).mkdir(parents=True)
        (self.workspace / "credentials/kubeconfig").write_text("fixture pinned kubeconfig")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for tool in ["kubectl", "helm", "aws", "kustomize", "systemctl", "nvidia-smi", "mount", "sudo"]:
            p = self.bin / tool
            p.write_text("#!" + sys.executable + "\n" + FAKE)
            p.chmod(0o700)
        node = {"metadata":{"name":"nks-worker","labels":{"kubernetes.io/hostname":"nks-worker"}},
                "spec":{}, "status":{"allocatable":{"nvidia.com/gpu":"1"}, "conditions":[{"type":"Ready","status":"True"}]}}
        secret = {"apiVersion":"v1","kind":"Secret","type":"Opaque",
                  "metadata":{"name":"aws-credentials","namespace":"eva-agent","uid":"secret-uid","resourceVersion":"1"},
                  "data":{k:base64.b64encode(v.encode()).decode() for k,v in {"AWS_ACCESS_KEY_ID":"AKIAfixtureDisabled", "AWS_SECRET_ACCESS_KEY":"private-fixture-do-not-print"}.items()}}
        values = {c:{"image":{"repository":OLD+"eva-"+c,"tag":"3.2.1"}} for c in ["app","agent","vision"]}
        values["iam"] = {"keycloak":{"image":{"repository":OLD+"eva-iam/keycloak","tag":"3.2.0"}}, "dispatcher":{"image":{"repository":OLD+"eva-iam/dispatcher","tag":"3.2.0"}}}
        values["vision"].update({"runtimeClassName":"nvidia","persistence":{"storageClass":"local-path"}})
        deployment = {"metadata":{"name":"eva-vision","uid":"deployment-uid", "annotations":{"meta.helm.sh/release-name":"eva-vision"}},
                      "spec":{"selector":{"matchLabels":{"app":"vision"}}, "strategy":{"type":"Recreate"},
                              "template":{"spec":{"containers":[{"name":"eva-vision","resources":{"limits":{"nvidia.com/gpu":"1","cpu":"8","memory":"36Gi"}}}]}}}}
        self.state = {"nodes":[node], "secret":secret, "values":values, "deployments":{"eva-vision":[deployment]}, "pods":{}}
        requested = {c:{"image":{"repository":NEW+"eva-"+c,"tag":"3.3.0"}} for c in ["app","agent","vision"]}
        requested["iam"]={"keycloak":{"image":{"repository":NEW+"eva-iam/keycloak","tag":"3.3.0"}},
                          "dispatcher":{"image":{"repository":NEW+"eva-iam/dispatcher","tag":"3.3.0"}}}
        requested["vision"].update(runtimeClassName="", modelCache={"storageClass":"nks-block"},
                                   resources={"limits":{"cpu":"8","memory":"36Gi","nvidia.com/gpu":"1"}})
        requested["agent"]["eva_agent_deploy"]={"vllm_profile":"L40sx1",
            "vllm_values":{"sharedPvcStorage":{"mode":"dynamic","storageClass":"nks-nas"}},
            "qdrant_values":{"persistence":{"storageClassName":"nks-block"},
                              "snapshotPersistence":{"storageClassName":"nks-block"}}}
        for component, v in requested.items():
            (self.workspace / "site-values" / (component+".yaml")).write_text(yaml.safe_dump({"controller":v}))
        (self.root / "state.json").write_text(json.dumps(self.state))
        self.env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ["PATH"], HOME=str(self.root),
                        EVA_REPO_ROOT=str(self.release), CLUSTER_FIXTURE=str(self.root),
                        ANSIBLE_ROLES_PATH=str(ROOT/"src/infra/roles")+":"+str(ROOT/"src/solution/roles"),
                        ANSIBLE_PYTHON_INTERPRETER=sys.executable, ANSIBLE_NOCOLOR="1", ANSIBLE_LOG_PATH=os.devnull,
                        KUBECONFIG=str(self.workspace/"credentials/kubeconfig"))
        self.variables = {"eva_managed_cluster":True,"ansible_become":False,"eva_site_id":"nks-test",
                          "eva_workspace_root":str(self.workspace),"eva_cluster_kubeconfig":self.env["KUBECONFIG"],
                          "eva_enabled_components":"app,iam,agent,vision", "repository_mode":"online_repository"}

    def incoming(self):
        (self.workspace/"credentials/aws_key.ini").write_text("aws_access_key_id = AKIAfixtureIncoming\nsecret_unused = ignored\naws_secret_access_key = private-incoming-do-not-print\n")

    def execute(self, playbook, success=True):
        r = subprocess.run([ANSIBLE, "-i", "controller,", "-c", "local", str(playbook), "-e", json.dumps(self.variables)],
                           env=self.env, text=True, capture_output=True, timeout=120)
        self.assertEqual(r.returncode==0,success,r.stdout+r.stderr)
        self.assertNotIn("private-incoming-do-not-print",r.stdout+r.stderr)
        self.assertNotIn("private-fixture-do-not-print",r.stdout+r.stderr)
        return r

    def calls(self):
        return [json.loads(line) for line in (self.root/"calls.jsonl").read_text().splitlines()]

    def test_disabled_shared_key_fails_common_precondition_before_mutations(self):
        self.execute(ROOT/"src/infra/playbooks/site_precondition.yaml",success=False)
        calls=self.calls()
        self.assertTrue(any(c["tool"]=="aws" for c in calls))
        self.assertFalse(any("patch" in c["args"] or "apply" in c["args"] for c in calls))
        self.assertTrue(all(c["kubeconfig"]==self.env["KUBECONFIG"] for c in calls if c["tool"]=="kubectl"))

    def test_old_registry_renewal_settings_fail_before_workload_changes(self):
        self.incoming()
        for component, setting in [('iam', {'imagePullSecrets':{'account':'339713051385'}}),
                                   ('agent', {'ecrRefresh':{'registry':OLD.split('/')[0]}})]:
            with self.subTest(component=component):
                self.variables['eva_enabled_components']=component
                source=self.workspace/'site-values'/(component+'.yaml')
                values=yaml.safe_load(source.read_text())
                values['controller'].update(setting)
                source.write_text(yaml.safe_dump(values))
                result=self.execute(ROOT/'src/infra/playbooks/site_precondition.yaml',success=False)
                self.assertIn('must match the selected ECR image repository',result.stdout)
                self.assertFalse(any('patch' in c['args'] or 'scale' in c['args'] or 'upgrade' in c['args'] for c in self.calls()))

    def test_new_solution_uses_filled_installer_values_without_existing_helm_release(self):
        self.incoming()
        self.state["new_install"]=True
        (self.root/"state.json").write_text(json.dumps(self.state))
        self.execute(ROOT/"src/infra/playbooks/site_precondition.yaml")
        self.execute(ROOT/"src/solution/playbooks/site_eva_config.yaml")
        self.assertFalse(any(c["tool"]=="helm" and c["args"][:2]==["get","values"] for c in self.calls()))

    def test_installed_solution_does_not_supply_missing_installer_values(self):
        self.incoming()
        (self.workspace/"site-values/app.yaml").unlink()
        self.execute(ROOT/"src/infra/playbooks/site_precondition.yaml",success=False)
        self.assertFalse(any(c["tool"]=="helm" and c["args"][:2]==["get","values"] for c in self.calls()))

    def test_current_roles_validate_new_registry_and_generate_config_without_host_setup(self):
        self.incoming()
        iam = {"metadata":{"name":"eva-iam-postgres","uid":"postgres-uid", "annotations":{"meta.helm.sh/release-name":"eva-iam"}},
               "spec":{"selector":{"matchLabels":{"app":"iam-db"}}, "template":{"spec":{
                   "containers":[{"name":"postgres"}], "volumes":[{"name":"data","hostPath":{"path":"//eva-iam/postgres"}}]}}}}
        source=self.workspace/"site-values/iam.yaml"
        iam_values=yaml.safe_load(source.read_text())
        iam_values["controller"]["persistence"]={"type":"hostPath", "hostPath":{"path":"/eva-iam"}}
        source.write_text(yaml.safe_dump(iam_values))
        self.state["deployments"]["eva-iam"] = [iam]
        self.state["pods"]["eva-iam"] = [{"metadata":{"labels":{"app":"iam-db"}}, "spec":{"nodeName":"nks-worker"}}]
        (self.root / "state.json").write_text(json.dumps(self.state))
        self.execute(ROOT/"src/infra/playbooks/site_precondition.yaml")
        self.execute(ROOT/"src/solution/playbooks/site_eva_config.yaml")
        context=yaml.safe_load((self.release/"out/work/config/nks-test/controller/context.yaml").read_text())
        self.assertEqual(context["eva_cluster_values"]["vision"]["runtimeClassName"],"")
        self.assertEqual(context["eva_cluster_values"]["vision"]["modelCache"]["storageClass"],"nks-block")
        self.assertEqual(context["eva_cluster_values"]["agent"]["image"]["repository"],NEW+"eva-agent")
        self.assertEqual(context["eva_cluster_values"]["vision"]["resources"]["limits"]["memory"],"36Gi")
        self.assertEqual(context["eva_cluster_iam_node_selectors"]["database"],{"kubernetes.io/hostname":"nks-worker"})
        report=self.release/"out/work/config/nks-test/controller/eva.yaml"
        self.assertEqual(yaml.safe_load(report.read_text())["gpu_count"],1)
        calls=self.calls()
        images=[c for c in calls if c["tool"]=="aws" and "batch-get-image" in c["args"]]
        self.assertEqual(len(images),5)
        self.assertTrue(all(c["incoming"] and "211722900905" in c["args"] for c in images))
        self.assertFalse(any(c["tool"] == "sudo" or c["tool"] == "nvidia-smi" or (c["tool"] == "systemctl" and any(a in c["args"] for a in ["stop", "start", "enable", "disable"])) or (c["tool"] == "mount" and "-l" not in c["args"]) for c in calls))

    def test_agent_uses_common_dependency_installs_with_nks_inputs_and_credential_repair(self):
        self.incoming()
        work=self.root/"agent-work"
        for component in ["vllm","qdrant"]:
            (work/("eva-agent-"+component)).mkdir(parents=True)
            (work/("eva-agent-"+component)/"effective-input-values.yaml").write_text("{}\n")
        self.variables.update(eva_enabled_components="agent",eva_agent_vllm_profile="L40sx1",
            eva_agent_workdir=str(work), eva_agent_namespace="eva-agent", eva_agent_kubeconfig=self.env["KUBECONFIG"],
            eva_agent_vllm_chart_version="3.3.0",eva_agent_qdrant_chart_version="1.18.2",airgap_mode=False,
            eva_agent_image_repository=NEW+"eva-agent",eva_agent_image_tag="3.3.0",
            eva_agent_sync_aws_credentials=True,eva_agent_aws_profile="default",
            eva_agent_aws_credentials_secret_name="aws-credentials",
            eva_agent_qdrant_snapshot_source="local_pv",
            eva_agent_aws_credentials_file_override=str(self.workspace/"credentials/aws_key.ini"),
            eva_agent_qdrant_post_renderer_arg="--post-renderer fixture-qdrant-renderer",
            eva_versions_file=str(ROOT/"src/solution/version.yaml"))
        names={"Ensure sa-eva-agent serviceaccount exists","Create sa-eva-agent serviceaccount when missing",
               "Validate exact NKS dependency manifests before changing workloads",
               "Install/Upgrade Qdrant (online)","Install/Upgrade eva-agent-vllm (online)"}
        shared=[t for t in yaml.safe_load((ROOT/"src/solution/roles/eva_agent/tasks/dependencies.yaml").read_text()) if t.get("name") in names]
        self.assertEqual(len(shared),5)
        p=self.root/"dependency.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all","connection":"local", "vars_files":[str(ROOT/"src/playbook-vars.yaml")],
            "pre_tasks":[{"ansible.builtin.import_tasks":str(ROOT/"src/playbook-preflight.yaml")}],
            "tasks":[{"ansible.builtin.import_role":{"name":"eva_agent","tasks_from":"workspace_values"}},
                     {"ansible.builtin.import_role":{"name":"eva_agent","tasks_from":"dependencies_cluster"}},
                     {"block":[{"ansible.builtin.import_role":{"name":"eva_agent","tasks_from":"credentials"}}]+shared,
                      "always":[{"ansible.builtin.include_role":{"name":"eva_agent","tasks_from":"resume_ecr"},
                                 "when":"eva_agent_ecr_suspended | default([]) | length > 0"}]}]}]))
        self.execute(p)
        state=json.loads((self.root/"state.json").read_text())
        self.assertEqual(base64.b64decode(state["secret"]["data"]["AWS_ACCESS_KEY_ID"]).decode(),"AKIAfixtureIncoming")
        calls=self.calls()
        upgrades=[c for c in calls if c["tool"]=="helm" and c["args"][:2]==["upgrade","--install"]]
        self.assertEqual({c["args"][2] for c in upgrades},{"eva-agent-vllm","eva-agent-qdrant"})
        self.assertTrue(all(c["args"][c["args"].index("--namespace")+1]=="eva-agent" for c in upgrades))
        renders=[c for c in calls if c["tool"]=="helm" and c["args"][0]=="template"]
        self.assertEqual({c["args"][1] for c in renders},{"eva-agent-vllm","eva-agent-qdrant"})
        self.assertLess(calls.index(renders[-1]),calls.index(upgrades[0]))
        self.assertFalse(any(c["tool"]=="helm" and "eva-agent-init" in c["args"] for c in calls))
        self.assertFalse(any(c["tool"] == "sudo" or c["tool"] == "nvidia-smi" or (c["tool"] == "systemctl" and any(a in c["args"] for a in ["stop", "start", "enable", "disable"])) or (c["tool"] == "mount" and "-l" not in c["args"]) for c in calls))

    def test_vllm_verification_preserves_nks_csi_binding_without_local_nfs(self):
        claim='eva-agent-vllm-shared-pvc-storage-claim'
        pvc={'metadata':{'name':claim,'uid':'cache-uid'},'status':{'phase':'Bound'},
             'spec':{'volumeName':'nks-csi-volume','storageClassName':'nks-nas',
                     'resources':{'requests':{'storage':'50Gi'}},'accessModes':['ReadWriteMany']}}
        self.state['vllm_pvc']=pvc
        self.state['vllm_pv']={'status':{'phase':'Bound'},'spec':{'csi':{'driver':'nas.csi.ncloud.com'},
            'claimRef':{'uid':'cache-uid','name':claim,'namespace':'eva-agent'}}}
        self.state['deployments']['eva-agent']=[{'metadata':{'labels':{'app.kubernetes.io/instance':'eva-agent-vllm'}},
            'spec':{'template':{'spec':{'volumes':[{'name':'cache','persistentVolumeClaim':{'claimName':claim}}]}}}}]
        (self.root/'state.json').write_text(json.dumps(self.state))
        self.variables.update(eva_agent_namespace='eva-agent',eva_agent_kubeconfig=self.env['KUBECONFIG'],
            eva_agent_cluster_vllm_values={'sharedPvcStorage':{'storageClass':'nks-nas'}},
            eva_agent_vllm_effective_values={'sharedPvcStorage':{'size':'50Gi'}})
        p=self.root/'verify-vllm.yaml'
        p.write_text(yaml.safe_dump([{'hosts':'all','connection':'local','tasks':[
            {'ansible.builtin.include_role':{'name':'eva_agent','tasks_from':'verify_vllm_reconciliation'}}]}]))
        for uid,success in [('',True),('cache-uid',True),('replaced-cache-uid',False)]:
            with self.subTest(previous_uid=uid):
                before=copy.deepcopy(pvc)
                before['metadata']['uid']=uid
                self.variables['eva_agent_cluster_vllm_pvc_before']={'stdout':json.dumps(before) if uid else ''}
                self.execute(p,success=success)
        self.assertFalse(any(c['tool']=='mount' or 'eva-agent-vllm-shared-pvc-storage' in c['args'] for c in self.calls()))

    def test_vision_uses_existing_auth_helper_and_cluster_renewal_without_systemd(self):
        self.incoming()
        self.variables.update(eva_enabled_components="vision",eva_vision_namespace="eva-vision",
                              eva_versions_file=str(ROOT/"src/solution/version.yaml"),
                              eva_vision_runtime_values={"image":{"repository":NEW+"eva-vision"},
                                "imagePullSecrets":{"enabled":True,"create":False,"ExistingSecret":"ecr-registry","region":"ap-northeast-2"}})
        p=self.root/"vision-credentials.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all","connection":"local", "vars_files":[str(ROOT/"src/playbook-vars.yaml")],
            "pre_tasks":[{"ansible.builtin.import_tasks":str(ROOT/"src/playbook-preflight.yaml")}],
            "tasks":[{"ansible.builtin.include_role":{"name":"eva_vision","tasks_from":"image_pull_secret"}}]}]))
        self.execute(p)
        state=json.loads((self.root/"state.json").read_text())
        token=json.loads(base64.b64decode(state["pull_secret"]["data"][".dockerconfigjson"]))
        self.assertIn(NEW.split('/')[0],token["auths"])
        renewal=[r for r in yaml.safe_load_all(state["renewal"]) if r["kind"]=="CronJob"][0]
        self.assertEqual(renewal["spec"]["schedule"],"0 */6 * * *")
        pod=renewal["spec"]["jobTemplate"]["spec"]["template"]["spec"]
        self.assertEqual(pod["initContainers"][0]["envFrom"][0]["secretRef"]["name"],"aws-credentials")
        self.assertFalse(any(c["tool"]=="systemctl" and any(a in c["args"] for a in ["stop","start","enable"]) for c in self.calls()))

    def test_app_seeds_chart_selected_pull_secret_and_checks_selected_image_tag(self):
        self.incoming()
        self.variables.update(eva_enabled_components="app", eva_online_component="app", eva_online_action="prepare",
                              eva_versions_file=str(ROOT/"src/solution/version.yaml"),
                              eva_online_input_values={"image":{"repository":NEW+"eva-app", "tag":"3.3.0-custom"},
                                                       "imagePullSecrets":{"create":True, "nameOverride":"app-pull-selected"}})
        p=self.root/"app-credentials.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all","connection":"local", "vars_files":[str(ROOT/"src/playbook-vars.yaml")],
            "pre_tasks":[{"ansible.builtin.import_tasks":str(ROOT/"src/playbook-preflight.yaml")}],
            "tasks":[{"ansible.builtin.include_tasks":str(ROOT/"src/solution/roles/eva_cluster_context/tasks/online_credentials.yaml")}]}]))
        self.execute(p)
        state=json.loads((self.root/"state.json").read_text())
        self.assertEqual(state["pull_secret"]["metadata"]["name"],"app-pull-selected")
        images=[c for c in self.calls() if c["tool"]=="aws" and "batch-get-image" in c["args"]]
        self.assertTrue(images)
        self.assertTrue(all("imageTag=3.3.0-custom" in c["args"] for c in images))

    def test_shared_iam_handoff_is_written_privately_by_kubeconfig_owner_without_sudo(self):
        state_root=self.root/"private-state"
        handoff=state_root/"nks-test"/"controller"/"iam-to-app.yaml"
        self.variables.update(eva_iam_write_app_config=True, eva_persistent_state_root=str(state_root),
                              eva_site_state_root=str(state_root/"nks-test"), eva_iam_app_handoff_dir=str(handoff.parent),
                              eva_iam_app_handoff_path=str(handoff), eva_iam_values_host="controller",
                              eva_iam_effective_host="iam.example.test", eva_iam_app_sso_base_url="https://iam.example.test",
                              eva_iam_realm="eva", eva_iam_app_client_id="eva-app", eva_iam_admin_api_client_id="eva-admin-api",
                              eva_iam_admin_api_client_secret="fixture-client-secret-do-not-print")
        names={"Ensure persistent IAM handoff directories exist on control node",
               "Write eva-iam.yaml (eva-app SSO hand-off) on control node"}
        tasks=[t for t in yaml.safe_load((ROOT/"src/solution/roles/eva_iam/tasks/main.yaml").read_text()) if t.get("name") in names]
        self.assertEqual(len(tasks),2)
        p=self.root/"iam-handoff.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all", "connection":"local", "tasks":tasks}]))
        result=self.execute(p)
        self.assertNotIn("fixture-client-secret-do-not-print", result.stdout+result.stderr)
        self.assertEqual(handoff.stat().st_uid, os.geteuid())
        self.assertEqual(handoff.stat().st_mode & 0o777, 0o600)
        self.assertEqual(handoff.parent.stat().st_mode & 0o777, 0o700)
        values=yaml.safe_load(handoff.read_text())
        self.assertEqual(values["target"],"controller")
        self.assertEqual(values["sso"]["adminClientSecret"],"fixture-client-secret-do-not-print")


spec=importlib.util.spec_from_file_location("cluster_guard",ROOT/"src/solution/roles/eva_cluster_guard/files/guard_cluster.py")
guard=importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class GuardTests(unittest.TestCase):
    def fixtures(self):
        deployment={"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"eva-vision"},
                    "spec":{"replicas":1,"strategy":{"type":"Recreate"},"template":{"spec":{"containers":[{"name":"eva-vision","resources":{"limits":{"nvidia.com/gpu":1}}}]}}}}
        node={"metadata":{"name":"nks-worker","labels":{}},"spec":{},"status":{"allocatable":{"nvidia.com/gpu":"1"},"conditions":[{"type":"Ready","status":"True"}]}}
        query=lambda kind:[node] if kind=="nodes" else []
        return deployment,query

    def test_nks_gpu_replacement_without_runtimeclass_and_unselected_ingress(self):
        deployment,query=self.fixtures()
        guard.validate({"component":"vision"},[deployment],[deployment],query)
        other={"metadata":{"namespace":"eva-agent","name":"model"}, "spec":{"nodeName":"nks-worker","containers":[{"resources":{"limits":{"nvidia.com/gpu":1}}}]},"status":{"phase":"Running"}}
        with self.assertRaisesRegex(RuntimeError,"Insufficient"):
            guard.validate({"component":"vision"},[deployment],[deployment],lambda kind:[other] if kind=="pods" else query(kind))

    def test_storage_path_changes_fail_before_role_stops_workloads(self):
        before,query=self.fixtures()
        before["spec"]["template"]["spec"]["volumes"]=[{"name":"data","hostPath":{"path":"//eva-iam/postgres"}}]
        after=copy.deepcopy(before)
        after["spec"]["template"]["spec"]["volumes"][0]["hostPath"]["path"]="/home/bastion/new-data"
        with self.assertRaisesRegex(RuntimeError,"volume changes"):
            guard.validate({"component":"iam"},[after],[before],query)

    def test_gpu_node_taint_and_required_affinity_are_checked_before_replacement(self):
        deployment,query=self.fixtures()
        node=query("nodes")[0]
        node["metadata"]["labels"]={"pool":"gpu"}
        node["spec"]["taints"]=[{"key":"dedicated", "value":"gpu", "effect":"NoSchedule"}]
        actual=lambda kind:[node] if kind=="nodes" else []
        with self.assertRaisesRegex(RuntimeError,"Insufficient"):
            guard.validate({"component":"vision"},[deployment],[deployment],actual)
        pod=deployment["spec"]["template"]["spec"]
        pod["tolerations"]=[{"key":"dedicated", "value":"gpu", "effect":"NoSchedule"}]
        guard.validate({"component":"vision"},[deployment],[deployment],actual)
        pod["affinity"]={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{
            "matchExpressions":[{"key":"pool","operator":"In","values":["other"]}]}]}}}
        with self.assertRaisesRegex(RuntimeError,"Insufficient"):
            guard.validate({"component":"vision"},[deployment],[deployment],actual)

    def test_dependency_namespace_and_gpu_allocation_use_dependency_release(self):
        deployment,query=self.fixtures()
        deployment["metadata"]["name"]="model"
        previous={"metadata":{"namespace":"eva-agent","labels":{"app.kubernetes.io/instance":"eva-agent-vllm"}},
                  "spec":{"nodeName":"nks-worker","containers":[{"resources":{"limits":{"nvidia.com/gpu":1}}}]},
                  "status":{"phase":"Running"}}
        guard.validate({"component":"vllm","namespace":"eva-agent","release":"eva-agent-vllm"},
                       [deployment],[],lambda kind:[previous] if kind=="pods" else query(kind))

    def test_qdrant_claim_template_change_and_unknown_class_fail(self):
        before={"kind":"StatefulSet","metadata":{"name":"eva-agent-qdrant"},"spec":{"replicas":1,
            "volumeClaimTemplates":[{"metadata":{"name":"storage"},"spec":{"storageClassName":"nks-block"}}],
            "template":{"spec":{"containers":[]}}}}
        after=copy.deepcopy(before)
        after["spec"]["volumeClaimTemplates"][0]["spec"]["storageClassName"]="new-block"
        with self.assertRaisesRegex(RuntimeError,"claim templates"):
            guard.validate({"component":"qdrant","namespace":"eva-agent"},[after],[before],lambda kind:[])
        with self.assertRaisesRegex(RuntimeError,"Unknown cluster StorageClass"):
            guard.validate({"component":"qdrant","namespace":"eva-agent"},[after],[],lambda kind:[])


if __name__=="__main__":
    unittest.main(verbosity=2)
