#!/usr/bin/env python3
"""Exercise shared Ansible preconditions/dependencies against an isolated NKS fixture."""
import base64
import copy
import functools
import http.server
import importlib.util
import json
import os
from pathlib import Path
import shutil
import site
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

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
 if args[0]=='template' and args[1]=='eva-agent-init':
  import yaml
  values=yaml.safe_load(pathlib.Path(args[args.index('--values')+1]).read_text())
  print(yaml.safe_dump_all([{'apiVersion':'storage.k8s.io/v1','kind':'StorageClass',
   'metadata':{'name':v['name']},'provisioner':v['provisioner'],'parameters':v.get('parameters',{}),
   'reclaimPolicy':'Retain','volumeBindingMode':'WaitForFirstConsumer'} for v in values['storageClass'].values()]))
  sys.exit(0)
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
        # Keep pip --user imports available while isolating HOME for the fixture.
        self.env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ["PATH"], HOME=str(self.root),
                        PYTHONUSERBASE=site.getuserbase(),
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
        (work/"eva-agent-init").mkdir()
        (work/"eva-agent-init/values-ncp.yaml").write_text(yaml.safe_dump({'storageClass':{
            'blockStorage':{'provisioner':'blk.csi.ncloud.com'},
            'fileSystem':{'provisioner':'nas.csi.ncloud.com','parameters':{'zoneNo':'2'}}}}))
        self.variables.update(eva_enabled_components="agent",eva_agent_vllm_profile="L40sx1",
            eva_agent_workdir=str(work), eva_agent_namespace="eva-agent", eva_agent_kubeconfig=self.env["KUBECONFIG"],
            eva_agent_vllm_chart_version="3.3.0",eva_agent_qdrant_chart_version="1.18.2",airgap_mode=False,
            eva_agent_image_repository=NEW+"eva-agent",eva_agent_image_tag="3.3.0",
            eva_agent_sync_aws_credentials=True,eva_agent_aws_profile="default",
            eva_agent_aws_credentials_secret_name="aws-credentials",
            eva_agent_qdrant_snapshot_source="s3",
            eva_agent_init_chart_version="1.0.0",eva_agent_init_values_file="values-ncp.yaml",
            eva_agent_aws_credentials_file_override=str(self.workspace/"credentials/aws_key.ini"),
            eva_agent_qdrant_post_renderer_arg="--post-renderer fixture-qdrant-renderer",
            eva_versions_file=str(ROOT/"src/solution/version.yaml"))
        names={"Ensure sa-eva-agent serviceaccount exists","Create sa-eva-agent serviceaccount when missing",
               "Create missing NKS CSI StorageClasses from the NCP init profile",
               "Validate exact NKS dependency manifests before changing workloads",
               "Install/Upgrade Qdrant (online)","Install/Upgrade eva-agent-vllm (online)"}
        shared=[t for t in yaml.safe_load((ROOT/"src/solution/roles/eva_agent/tasks/dependencies.yaml").read_text()) if t.get("name") in names]
        self.assertEqual(len(shared),6)
        for task in shared:
            if task.get("ansible.builtin.include_tasks") == "init_cluster.yaml":
                task["ansible.builtin.include_tasks"] = str(ROOT/"src/solution/roles/eva_agent/tasks/init_cluster.yaml")
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
        self.assertEqual({c["args"][1] for c in renders},{"eva-agent-init","eva-agent-vllm","eva-agent-qdrant"})
        self.assertLess(calls.index(renders[-1]),calls.index(upgrades[0]))
        self.assertEqual(renders[0]["args"][1],"eva-agent-init")
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

    def test_nks_gpu_support_does_not_require_a_provider_daemonset_name(self):
        names={"Check NVIDIA device plugin daemonset exists","Validate NVIDIA device plugin prerequisite"}
        tasks=[t for t in yaml.safe_load((ROOT/"src/solution/roles/eva_vision/tasks/main.yaml").read_text()) if t.get("name") in names]
        self.assertEqual(len(tasks),2)
        p=self.root/"vision-gpu-prerequisite.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all","connection":"local","tasks":tasks}]))
        result=self.execute(p)
        self.assertIn("skipped=2",result.stdout)
        self.assertFalse(any("daemonset" in call["args"] for call in self.calls()))
        self.variables["eva_managed_cluster"]=False
        self.variables["eva_vision_kubeconfig"]=self.env["KUBECONFIG"]
        self.execute(p,success=False)
        self.assertTrue(any("daemonset" in call["args"] for call in self.calls()))

    def test_vision_ncp_profile_is_loaded_before_workspace_and_cli_overrides(self):
        public=self.root/"public-profiles"
        public.mkdir()
        # Deliberately differ from the generated server defaults, so a passing
        # assertion proves the fetched profile participates in the real merge.
        profile={'image':{'repository':NEW+'eva-vision','tag':'3.3.0'},
            'modelSource':{'type':'s3'},'s3':{'existingSecret':'eva-vision-s3'},
            'modelCache':{'storageClass':'nks-block-storage'},
            'resources':{kind:{'cpu':'6','memory':'48Gi','nvidia.com/gpu':1} for kind in ['limits','requests']},
            'gpu':{'count':1,'mig':{'resourceName':''},'mps':{'enabled':False}}}
        (public/"eva-vision-ncp.yaml").write_text(yaml.safe_dump(profile))
        class QuietHandler(http.server.SimpleHTTPRequestHandler):
            def log_message(self, *_): pass
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),functools.partial(QuietHandler,directory=str(public)))
        threading.Thread(target=server.serve_forever,daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        definitions=yaml.safe_load((ROOT/'src/solution/roles/eva_vision/tasks/main.yaml').read_text())
        source_names={'Download the published NCP Vision Values','Read the published NCP Vision Values',
                      'Parse the published NCP Vision Values','Validate the NCP Vision Values document'}
        sources=[t for t in definitions if t.get('name') in source_names]
        merge=next(t for t in definitions if t.get('name')=='Build effective EVA Vision Helm input values')
        metadata=next(t for t in definitions if t.get('name')=='Record EVA Vision values source metadata on control node')
        for index,(managed,workspace,cli,missing,cpu,memory) in enumerate([
                (True,{}, {},False,'6','48Gi'),
                (True,{'resources':{'limits':{'cpu':'7','memory':'50Gi'}}}, {},False,'7','50Gi'),
                (True,{'resources':{'limits':{'cpu':'7','memory':'50Gi'}}},
                 {'resources':{'limits':{'cpu':'9','memory':'52Gi'}}},False,'9','52Gi'),
                (False,{}, {},False,'4','24Gi'), (True,{}, {},True,None,None)]):
            with self.subTest(managed=managed, workspace=bool(workspace), cli=bool(cli), missing=missing):
                work=self.root/('vision-profile-'+str(index))
                work.mkdir()
                defaults=work/'chart.yaml'
                defaults.write_text(yaml.safe_dump({'resources':{'limits':{'ephemeral-storage':'30Gi'}},
                                                   'runtimeClassName':'nvidia'}))
                self.variables.update(eva_managed_cluster=managed,airgap_mode=False,
                    eva_vision_workdir=str(work),eva_vision_deploy_values_dir=str(work),
                    eva_vision_ncp_values_url='http://127.0.0.1:'+str(server.server_port)+'/'+('missing.yaml' if missing else 'eva-vision-ncp.yaml'),
                    eva_vision_chart_version='3.3.1',eva_vision_image_tag='3.3.1-beta.1',
                    eva_vision_image_repository=NEW+'eva-vision',eva_vision_gpu_count=1,
                    eva_vision_gpu_resource_name='nvidia.com/gpu',eva_vision_storage_class='local-path',eva_vision_pvc_size='20Gi',
                    repository_registry='',repository_project='eva',eva_vision_workspace_chart_values=workspace,
                    eva_vision_workspace_values=workspace,eva_vision_cli_values_override=cli)
                tasks=[{'ansible.builtin.set_fact':{'eva_vision_ncp_values':{}}}]+sources+[
                    {'ansible.builtin.template':{'src':str(ROOT/'src/solution/values/vision-k3s-override.yaml.j2'),
                                                'dest':str(work/'generated.yaml')}},
                    {'ansible.builtin.slurp':{'src':'{{ item }}'},'loop':[str(defaults),str(work/'generated.yaml')],
                     'register':'eva_vision_generated_values_raw'},merge,
                    {'ansible.builtin.copy':{'dest':str(work/'effective.yaml'),'content':'{{ eva_vision_effective_values | to_nice_yaml }}'}},metadata]
                playbook=work/'playbook.yaml'
                playbook.write_text(yaml.safe_dump([{'hosts':'all','connection':'local','gather_facts':False,'tasks':tasks}]))
                self.execute(playbook,success=not missing)
                if missing:
                    self.assertFalse((work/'effective.yaml').exists())
                    continue
                effective=yaml.safe_load((work/'effective.yaml').read_text())
                self.assertEqual(effective['resources']['limits']['cpu'],cpu)
                self.assertEqual(effective['resources']['limits']['memory'],memory)
                self.assertEqual(effective['resources']['limits']['ephemeral-storage'],'30Gi')
                self.assertEqual(effective['image']['tag'],'3.3.1-beta.1')
                self.assertEqual(effective['image']['repository'],NEW+'eva-vision')
                self.assertEqual(effective['runtimeClassName'],'' if managed else 'nvidia')
                self.assertEqual(effective['gpu']['mps']['enabled'],not managed)
                self.assertEqual(effective['modelCache']['storageClass'],'nks-block-storage' if managed else 'local-path')
                self.assertTrue(effective['modelCache']['enabled'])
                source=yaml.safe_load((work/'values-sources.yaml').read_text())
                self.assertEqual(source['environment_values_source'],self.variables['eva_vision_ncp_values_url'] if managed else '')

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

    def test_nks_handoff_and_database_backups_use_account_paths_without_sudo(self):
        # Exercise the production task-level privilege choice, without an
        # ansible_become extra var masking an accidental become: true.
        self.variables.pop("ansible_become")
        resolver=next(t for t in yaml.safe_load((ROOT/"src/solution/roles/eva_cluster_context/tasks/main.yaml").read_text())
                      if t.get("name")=="Resolve account-owned controller paths and the pinned Kubernetes connection")
        app_tasks=yaml.safe_load((ROOT/"src/solution/roles/eva_app/tasks/main.yaml").read_text())
        reads={"Check same-target IAM-to-App handoff","Read same-target IAM-to-App handoff",
               "Check Argo CD handoff receipt on control node","Read Argo CD handoff receipt on control node"}
        tasks=[resolver,*[t for t in app_tasks if t.get("name") in reads]]
        self.assertEqual(len(tasks),5)
        private=self.root/".local/state/eva"
        iam=private/"sites/nks-test/controller/eva-iam.yaml"
        argocd=private/"sites/nks-test/argocd-handoff.yaml"
        iam.parent.mkdir(parents=True,mode=0o700)
        iam.write_text("sso: {adminClientSecret: fixture-client-secret-do-not-print}\n");iam.chmod(0o600)
        argocd.write_text("schema_version: v1\n");argocd.chmod(0o600)
        tasks.append({"ansible.builtin.copy":{"dest":str(self.root/"paths.json"),"mode":"0600","content":
            '{{ {"site": eva_site_state_root, "database": eva_app_db_backup_root, "helm": eva_helm_values_backup_root, '
            '"iam": eva_app_iam_handoff_raw.content, "argocd": eva_app_argocd_handoff_raw.content} | to_json }}'},"no_log":True})
        self.variables.update(eva_app_workspace_sso_complete=False,
            eva_app_iam_handoff_path="{{ eva_site_state_root }}/controller/eva-iam.yaml",
            eva_app_argocd_handoff_path=str(argocd))
        p=self.root/"account-receipts.yaml"
        p.write_text(yaml.safe_dump([{"hosts":"all","connection":"local","vars_files":[str(ROOT/"src/playbook-vars.yaml")],"tasks":tasks}]))
        result=self.execute(p)
        self.assertNotIn("fixture-client-secret-do-not-print",result.stdout+result.stderr)
        paths=json.loads((self.root/"paths.json").read_text())
        self.assertEqual(paths["site"],str(private/"sites/nks-test"))
        self.assertEqual(paths["database"],str(private/"backups/app-db/nks-test/eva-app"))
        self.assertEqual(paths["helm"],str(private/"backups/helm-values"))
        self.assertEqual(base64.b64decode(paths["iam"]).decode(),iam.read_text())
        self.assertEqual(base64.b64decode(paths["argocd"]).decode(),argocd.read_text())
        self.assertFalse(any(call["tool"]=="sudo" for call in self.calls()))
        spec=importlib.util.spec_from_file_location("app_database",ROOT/"src/solution/roles/eva_app/files/database_transition.py")
        database=importlib.util.module_from_spec(spec);spec.loader.exec_module(database)
        backup=database.private_dir(paths["database"])
        self.assertEqual(backup.stat().st_uid,os.geteuid())
        self.assertEqual(backup.stat().st_mode & 0o777,0o700)


spec=importlib.util.spec_from_file_location("cluster_guard",ROOT/"src/solution/roles/eva_cluster_guard/files/guard_cluster.py")
guard=importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class InspectorTests(unittest.TestCase):
    def test_uninstalled_helm_history_is_a_fresh_install_but_pending_still_blocks(self):
        spec=importlib.util.spec_from_file_location("cluster_inspector",ROOT/"src/solution/roles/eva_cluster_context/files/inspect_cluster.py")
        inspector=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(inspector)
        for status in ["uninstalled","pending-upgrade"]:
            with self.subTest(status=status), tempfile.TemporaryDirectory() as directory:
                root=Path(directory)
                (root/"site-values").mkdir()
                (root/"site-values/agent.yaml").write_text(yaml.safe_dump({"controller":{"image":{"tag":"3.3.0"}}}))
                output=root/"context";output.mkdir()
                for component in ["agent","vllm","qdrant"]:
                    (output/(component+"-previous.yaml")).write_text("stale manifest")
                def read(command):
                    if command[:2]==["helm","list"]:
                        return json.dumps([{"name":name,"status":status} for name in ["eva-agent","eva-agent-vllm","eva-agent-qdrant"]])
                    if command[-3:]==["nodes","-o","json"]:
                        return json.dumps({"items":[{"metadata":{"name":"worker"},"status":{"conditions":[{"type":"Ready","status":"True"}],"allocatable":{}}}]})
                    if command==["kubectl","config","current-context"]:
                        return "fixture-context"
                    raise AssertionError("Unexpected cluster read: "+str(command))
                with patch.object(sys,"argv",["inspect_cluster.py","--workspace",str(root),"--host","controller","--output",str(output)]), \
                     patch.dict(os.environ,{"EVA_CLUSTER_COMPONENTS":"agent"}), \
                     patch.object(inspector,"run",side_effect=read),patch("builtins.print"):
                    if status=="uninstalled":
                        inspector.main()
                        self.assertFalse(list(output.glob("*-previous.yaml")))
                        self.assertEqual(yaml.safe_load((output/"context.yaml").read_text())["eva_cluster_values"]["agent"],{"image":{"tag":"3.3.0"}})
                    else:
                        with self.assertRaisesRegex(RuntimeError,"pending Helm operation"):
                            inspector.main()


class GuardTests(unittest.TestCase):
    def fixtures(self):
        deployment={"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"eva-vision"},
                    "spec":{"replicas":1,"strategy":{"type":"Recreate"},"template":{"spec":{"containers":[{"name":"eva-vision","resources":{"limits":{"nvidia.com/gpu":1}}}]}}}}
        node={"metadata":{"name":"nks-worker","labels":{}},"spec":{},"status":{"allocatable":{"nvidia.com/gpu":"1"},"conditions":[{"type":"Ready","status":"True"}]}}
        query=lambda kind,namespace=None,name=None:[node] if kind=="nodes" else []
        return deployment,query

    def test_null_environment_lists_preserve_required_secret_checks(self):
        # The IAM Dispatcher renders env: null when it has no extra env vars.
        # Empty fields must not prevent validation of the other references.
        secret={"kind":"Secret","metadata":{"name":"dispatcher-auth"}}
        for empty_field in ["env", "envFrom"]:
            with self.subTest(empty_field=empty_field):
                deployment,query=self.fixtures()
                container=deployment["spec"]["template"]["spec"]["containers"][0]
                container[empty_field]=None
                if empty_field=="env":
                    container["envFrom"]=[{"secretRef":{"name":"dispatcher-auth"}}]
                else:
                    container["env"]=[{"name":"PASSWORD","valueFrom":{
                        "secretKeyRef":{"name":"dispatcher-auth","key":"password"}}}]
                guard.validate({"component":"iam"},[deployment,secret],[],query)
                with self.assertRaisesRegex(RuntimeError,"Missing required Kubernetes resource"):
                    guard.validate({"component":"iam"},[deployment],[],query)

    def test_nullable_pod_lists_and_resource_maps_validate(self):
        deployment,query=self.fixtures()
        pod=deployment["spec"]["template"]["spec"]
        pod.update(initContainers=None,volumes=None,imagePullSecrets=None,
                   nodeSelector=None,tolerations=None,affinity=None)
        container=pod["containers"][0]
        container.update(env=None,envFrom=None,volumeMounts=None)
        guard.validate({"component":"vision"},[deployment],[deployment],query)
        for resources in [None, {"limits":None}]:
            with self.subTest(resources=resources):
                container["resources"]=resources
                container["env"]=[{"name":"EMPTY","valueFrom":None}]
                guard.validate({"component":"vision"},[deployment],[deployment],query)

    def test_nullable_affinity_does_not_ignore_required_placement(self):
        deployment,query=self.fixtures()
        pod=deployment["spec"]["template"]["spec"]
        for affinity in [None, {"nodeAffinity":None},
                         {"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":None}}]:
            with self.subTest(affinity=affinity):
                pod["affinity"]=affinity
                guard.validate({"component":"vision"},[deployment],[],query)
        pod["affinity"]={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{
            "nodeSelectorTerms":[{"matchExpressions":None,"matchFields":[{
                "key":"metadata.name","operator":"In","values":["other-node"]}]}]}}}
        self.assertTrue(any("GPU placement" in warning for warning in guard.validate({"component":"vision"},[deployment],[],query)))

    def test_nullable_node_metadata_does_not_hide_missing_gpu_capacity(self):
        deployment,query=self.fixtures()
        node=query("nodes")[0]
        node["metadata"]["labels"]=None
        node["spec"]=None
        actual=lambda kind,namespace=None,name=None:[node] if kind=="nodes" else []
        guard.validate({"component":"vision"},[deployment],[],actual)
        node["status"]["allocatable"]=None
        with self.assertRaisesRegex(RuntimeError,"Unknown cluster GPU resource"):
            guard.validate({"component":"vision"},[deployment],[],actual)

    def test_empty_statefulset_claim_templates_do_not_require_migration(self):
        before={"kind":"StatefulSet","metadata":{"name":"stateless"},
                "spec":{"volumeClaimTemplates":None,"template":{"spec":{"containers":[]}}}}
        after=copy.deepcopy(before)
        after["spec"]["volumeClaimTemplates"]=[]
        guard.validate({"component":"qdrant"},[after],[before],lambda kind,namespace=None,name=None:[])

    def test_null_mounts_do_not_allow_removing_persistent_mount(self):
        before,query=self.fixtures()
        pod=before["spec"]["template"]["spec"]
        pod["volumes"]=[{"name":"data","hostPath":{"path":"//eva-iam/postgres"}}]
        pod["containers"][0]["volumeMounts"]=[{"name":"data","mountPath":"/data"}]
        after=copy.deepcopy(before)
        after["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]=None
        with self.assertRaisesRegex(RuntimeError,"persistent mountPath"):
            guard.validate({"component":"iam"},[after],[before],query)

    def test_invalid_optional_field_and_parser_errors_do_not_expose_values(self):
        deployment,query=self.fixtures()
        deployment["spec"]["template"]["spec"]["containers"][0]["env"]="fixture-private-value"
        with self.assertRaisesRegex(RuntimeError,"Invalid Kubernetes field env") as caught:
            guard.validate({"component":"iam"},[deployment],[],query)
        self.assertNotIn("fixture-private-value",str(caught.exception))
        try:
            yaml.safe_load("secret: [fixture-private-value")
        except yaml.YAMLError as error:
            message=guard.error_message(error)
            self.assertIn(type(error).__name__,message)
            self.assertNotIn("fixture-private-value",message)

    def test_missing_tool_and_timeout_have_safe_actionable_errors(self):
        command=["helm","template","eva-iam","--set","secret=fixture-private-value"]
        for error,expected in [(FileNotFoundError("fixture-private-value"),"tool not found: helm"),
                               (subprocess.TimeoutExpired(command,180),"timed out: helm template eva-iam")]:
            with self.subTest(error=type(error).__name__), patch.object(guard.subprocess,"run",side_effect=error):
                with self.assertRaisesRegex(RuntimeError,expected) as caught:
                    guard.run(command)
                self.assertNotIn("fixture-private-value",str(caught.exception))

    def test_installed_diagnostic_only_reads_selected_manifests_and_cluster(self):
        deployment,query=self.fixtures()
        queries=[]
        def inspect(kind,namespace=None,environment=None,name=None):
            queries.append(kind)
            return query(kind)
        def read(command,environment=None):
            if command[:2]==["helm","list"]:
                name=command[command.index("--filter")+1].strip("^$")
                return json.dumps([{"name":name,"status":"deployed"}])
            return yaml.safe_dump(deployment)
        with patch.object(sys,"argv",["guard_cluster.py","--installed","iam","app","agent","vision","vllm","qdrant"]), \
             patch.object(guard,"run",side_effect=read) as commands, \
             patch.object(guard,"items",side_effect=inspect), patch("builtins.print") as output:
            guard.main()
        self.assertEqual([call.args[0] for call in commands.call_args_list if call.args[0][:3]==["helm","get","manifest"]],
            [["helm","get","manifest",name,"-n",ns] for name,ns in [
                ("eva-iam","eva-iam"),("eva-app","eva-app"),("eva-agent","eva-agent"),
                ("eva-vision","eva-vision"),("eva-agent-vllm","eva-agent"),("eva-agent-qdrant","eva-agent")]])
        self.assertEqual(output.call_count,6)
        self.assertEqual(queries,["nodes"])
        with patch.object(sys,"argv",["guard_cluster.py","--installed","iam"]), \
             patch.object(guard,"run",return_value="[]"),patch("builtins.print") as output:
            guard.main()
            self.assertIn("[SKIP]",output.call_args.args[0])

    def test_prepared_diagnostic_preserves_render_environment_and_cluster_identity(self):
        deployment,query=self.fixtures()
        environments=[{"KUBECONFIG":"cluster-a","HELM_PLUGINS":"plugins-a"},
                      {"KUBECONFIG":"cluster-b","HELM_PLUGINS":"plugins-b"}]
        queries=[]
        def inspect(kind,namespace=None,environment=None,name=None):
            queries.append((environment["KUBECONFIG"],kind))
            return query(kind)
        with tempfile.TemporaryDirectory() as directory:
            requests=[]
            for index,environment in enumerate(environments):
                request={"component":"iam","chart":"fixture-chart","version":"3.3.0",
                         "values":[],"set":[],"post_renderer":"fixture-renderer",
                         "previous":str(Path(directory)/(str(index)+"-previous.yaml")),"environment":environment}
                if index:
                    Path(request["previous"]).write_text(yaml.safe_dump(deployment))
                path=Path(directory)/(str(index)+".json")
                path.write_text(json.dumps(request));requests += ["--request",str(path)]
            with patch.object(sys,"argv",["guard_cluster.py",*requests]), \
                 patch.object(guard,"run",return_value=yaml.safe_dump(deployment)) as commands, \
                 patch.object(guard,"items",side_effect=inspect),patch("builtins.print"):
                guard.main()
        self.assertEqual([call.args[1] for call in commands.call_args_list],environments)
        self.assertEqual(set(queries),{("cluster-a","nodes"),("cluster-b","nodes")})
        self.assertTrue(all("--dry-run=server" in call.args[0] for call in commands.call_args_list))
        self.assertEqual(["--is-upgrade" in call.args[0] for call in commands.call_args_list],[False,True])

    def test_optional_global_rbac_does_not_block_but_required_namespace_reads_do(self):
        service={"kind":"Service","metadata":{"name":"app"},"spec":{"ports":[{"nodePort":32010}]}}
        deployment,_=self.fixtures()
        pod=deployment["spec"]["template"]["spec"]
        pod["containers"][0]["resources"]={}
        pod["imagePullSecrets"]=[{"name":"required-pull"}]
        with tempfile.TemporaryDirectory() as directory:
            request={"component":"app","chart":"fixture-chart","version":"3.3.0","values":[],"set":[],
                     "post_renderer":"","previous":str(Path(directory)/"missing.yaml")}
            path=Path(directory)/"request.json";path.write_text(json.dumps(request))
            for docs,blocked in [([service],False),([service,deployment],True)]:
                with self.subTest(blocked=blocked),patch.object(sys,"argv",["guard_cluster.py","--request",str(path)]), \
                     patch.object(guard,"run",return_value=yaml.safe_dump_all(docs)), \
                     patch.object(guard,"items",side_effect=RuntimeError("Cluster validation failed: kubectl get")), \
                     patch("builtins.print") as output:
                    if blocked:
                        with self.assertRaisesRegex(RuntimeError,"Cluster validation failed"):
                            guard.main()
                    else:
                        guard.main()
                        self.assertTrue(any("[WARN]" in call.args[0] for call in output.call_args_list))

    def test_nks_gpu_replacement_without_runtimeclass_and_unselected_ingress(self):
        deployment,query=self.fixtures()
        guard.validate({"component":"vision"},[deployment],[deployment],query)
        other={"metadata":{"namespace":"eva-agent","name":"model"}, "spec":{"nodeName":"nks-worker","containers":[{"resources":{"limits":{"nvidia.com/gpu":1}}}]},"status":{"phase":"Running"}}
        guard.validate({"component":"vision"},[deployment],[deployment],lambda kind,namespace=None,name=None:[other] if kind=="pods" else query(kind))

    def test_storage_path_changes_fail_before_role_stops_workloads(self):
        before,query=self.fixtures()
        before["spec"]["template"]["spec"]["volumes"]=[{"name":"data","hostPath":{"path":"//eva-iam/postgres"}}]
        before["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]=[{"name":"data","mountPath":"/data"}]
        after=copy.deepcopy(before)
        after["spec"]["template"]["spec"]["volumes"][0]["hostPath"]["path"]="/home/bastion/new-data"
        with self.assertRaisesRegex(RuntimeError,"Persistent workload"):
            guard.validate({"component":"iam"},[after],[before],query)

    def test_gpu_node_taint_and_required_affinity_are_checked_before_replacement(self):
        deployment,query=self.fixtures()
        node=query("nodes")[0]
        node["metadata"]["labels"]={"pool":"gpu"}
        node["spec"]["taints"]=[{"key":"dedicated", "value":"gpu", "effect":"NoSchedule"}]
        actual=lambda kind,namespace=None,name=None:[node] if kind=="nodes" else []
        self.assertTrue(guard.validate({"component":"vision"},[deployment],[deployment],actual))
        pod=deployment["spec"]["template"]["spec"]
        pod["tolerations"]=[{"key":"dedicated", "value":"gpu", "effect":"NoSchedule"}]
        guard.validate({"component":"vision"},[deployment],[deployment],actual)
        pod["affinity"]={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{
            "matchExpressions":[{"key":"pool","operator":"In","values":["other"]}]}]}}}
        self.assertTrue(guard.validate({"component":"vision"},[deployment],[deployment],actual))

    def test_dependency_refs_are_read_by_name_in_the_dependency_namespace(self):
        deployment,query=self.fixtures()
        deployment["metadata"]["name"]="model"
        deployment["spec"]["template"]["spec"]["containers"][0].update(resources={},envFrom=[{"secretRef":{"name":"model-auth"}}])
        reads=[]
        def inspect(kind,namespace=None,name=None):
            reads.append((kind,namespace,name))
            return [{"kind":"Secret","metadata":{"name":"model-auth","namespace":"eva-agent"}}] if namespace=="eva-agent" else []
        guard.validate({"component":"vllm","namespace":"eva-agent","release":"eva-agent-vllm"},
                       [deployment],[],inspect)
        self.assertEqual(reads,[("secrets","eva-agent","model-auth")])

    def test_new_cpu_workload_with_default_sa_and_hostpath_needs_no_extra_cluster_reads(self):
        deployment,_=self.fixtures()
        pod=deployment["spec"]["template"]["spec"]
        pod["containers"][0]["resources"]={}
        pod.update(serviceAccountName="default",volumes=[{"name":"data","hostPath":{"path":"/prepared/iam","type":"DirectoryOrCreate"}}])
        pod["containers"][0]["volumeMounts"]=[{"name":"data","mountPath":"/data"}]
        with patch.object(guard,"items",side_effect=AssertionError("unexpected inspection")):
            self.assertEqual(guard.validate({"component":"iam"},[deployment],[]),[])

    def test_preinstall_ecr_hook_provides_its_exact_custom_secret(self):
        deployment,query=self.fixtures()
        deployment["spec"]["template"]["spec"]["imagePullSecrets"]=[{"name":"custom-pull"}]
        hook={"kind":"Job","metadata":{"name":"eva-agent-ecr-refresh-hook","annotations":{"helm.sh/hook":"pre-install,pre-upgrade"}},
              "spec":{"template":{"spec":{"containers":[{"name":"refresh","env":[{"name":"ECR_SECRET_NAME","value":"custom-pull"}]}]}}}}
        guard.validate({"component":"agent"},[deployment,hook],[],query)
        deployment["spec"]["template"]["spec"]["imagePullSecrets"][0]["name"]="different-pull"
        with self.assertRaisesRegex(RuntimeError,"Missing required Kubernetes resource"):
            guard.validate({"component":"agent"},[deployment,hook],[],query)

    def test_statefulset_claims_are_created_by_kubernetes_after_guard(self):
        stateful={"kind":"StatefulSet","metadata":{"name":"qdrant"},"spec":{
            "volumeClaimTemplates":[{"metadata":{"name":"data"},"spec":{"resources":{"requests":{"storage":"10Gi"}}}}],
            "template":{"spec":{"containers":[{"name":"qdrant"}],"volumes":[{
                "name":"data","persistentVolumeClaim":{"claimName":"data-qdrant-0"}}]}}}}
        self.assertEqual(guard.validate({"component":"qdrant","namespace":"eva-agent"},[stateful],[],lambda kind,namespace=None,name=None:[]),[])

    def test_pending_pvc_is_allowed_but_lost_pvc_still_blocks(self):
        pvc={"kind":"PersistentVolumeClaim","metadata":{"name":"data"},"spec":{
            "storageClassName":"nas","accessModes":["ReadWriteMany"],"resources":{"requests":{"storage":"500Gi"}}}}
        claim=copy.deepcopy(pvc);claim["status"]={"phase":"Pending"}
        sc={"metadata":{"name":"nas"},"volumeBindingMode":"WaitForFirstConsumer"}
        query=lambda kind,namespace=None,name=None:[claim] if kind=="persistentvolumeclaims" else [sc] if kind=="storageclasses" else []
        self.assertTrue(any("Pending" in item for item in guard.validate({"component":"app"},[pvc],[pvc],query)))
        claim["status"]["phase"]="Lost"
        with self.assertRaisesRegex(RuntimeError,"lost its volume"):
            guard.validate({"component":"app"},[pvc],[pvc],query)

    def test_pvc_resize_obeys_kubernetes_rules_and_equivalent_units(self):
        before={"kind":"PersistentVolumeClaim","metadata":{"name":"data"},"spec":{
            "storageClassName":"block","accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}
        claim=copy.deepcopy(before);claim["status"]={"phase":"Bound"}
        for size,expansion,allowed in [("1024Mi",False,True),("2Gi",True,True),("2Gi",False,False),("512Mi",True,False)]:
            with self.subTest(size=size,expansion=expansion):
                after=copy.deepcopy(before);after["spec"]["resources"]["requests"]["storage"]=size
                sc={"metadata":{"name":"block"},"allowVolumeExpansion":expansion}
                query=lambda kind,namespace=None,name=None:[claim] if kind=="persistentvolumeclaims" else [sc] if kind=="storageclasses" else []
                if allowed: guard.validate({"component":"app"},[after],[before],query)
                else:
                    with self.assertRaises(RuntimeError):guard.validate({"component":"app"},[after],[before],query)

    def test_lookup_retained_pvc_can_disappear_from_new_manifest_without_deletion(self):
        pvc={"kind":"PersistentVolumeClaim","metadata":{"name":"data","annotations":{"helm.sh/resource-policy":"keep"}},
             "spec":{"resources":{"requests":{"storage":"500Gi"}}},"status":{"phase":"Bound"}}
        query=lambda kind,namespace=None,name=None:[pvc] if kind=="persistentvolumeclaims" else []
        guard.validate({"component":"app"},[],[pvc],query)
        pvc["metadata"]["annotations"]={}
        with self.assertRaisesRegex(RuntimeError,"remove existing storage"):
            guard.validate({"component":"app"},[],[pvc],query)

    def test_pv_capacity_increase_preserves_data_but_csi_change_still_blocks(self):
        before={"kind":"PersistentVolume","metadata":{"name":"data"},"spec":{
            "capacity":{"storage":"1Gi"},"csi":{"driver":"nas.csi.ncloud.com","volumeHandle":"fs-original"}}}
        after=copy.deepcopy(before);after["spec"]["capacity"]["storage"]="2Gi"
        guard.validate({"component":"app"},[after],[before],lambda kind,namespace=None,name=None:[])
        after["spec"]["csi"]["volumeHandle"]="fs-different"
        with self.assertRaisesRegex(RuntimeError,"separate migration"):
            guard.validate({"component":"app"},[after],[before],lambda kind,namespace=None,name=None:[])

    def test_static_pv_binding_does_not_require_a_storageclass_object(self):
        pv={"kind":"PersistentVolume","metadata":{"name":"static"},"spec":{"storageClassName":"manual-class"}}
        pvc={"kind":"PersistentVolumeClaim","metadata":{"name":"data"},"spec":{"storageClassName":"manual-class"}}
        guard.validate({"component":"app"},[pv,pvc],[],lambda kind,namespace=None,name=None:[])
        pvc["spec"]["volumeName"]="external-static"
        guard.validate({"component":"app"},[pvc],[],lambda kind,namespace=None,name=None:[])

    def test_mount_renaming_default_fields_and_double_slashes_preserve_data(self):
        before,query=self.fixtures()
        pod=before["spec"]["template"]["spec"]
        pod["volumes"]=[{"name":"old-data","hostPath":{"path":"//eva-iam/postgres","type":"DirectoryOrCreate"}}]
        pod["containers"][0]["volumeMounts"]=[{"name":"old-data","mountPath":"/data"}]
        after=copy.deepcopy(before);pod=after["spec"]["template"]["spec"]
        pod["volumes"]=[{"name":"new-data","hostPath":{"path":"/eva-iam/postgres","type":"Directory"}}]
        pod["containers"][0]["volumeMounts"]=[{"name":"new-data","mountPath":"/data/","readOnly":False}]
        guard.validate({"component":"iam"},[after],[before],query)

    def test_fresh_gpu_deployment_allows_rollingupdate_and_retries_do_not_block(self):
        deployment,query=self.fixtures()
        deployment["spec"]["strategy"]={"type":"RollingUpdate"}
        self.assertEqual(guard.validate({"component":"vision"},[deployment],[],query),[])
        self.assertTrue(guard.validate({"component":"vision"},[deployment],[deployment],query))

    def test_qdrant_claim_template_change_and_unknown_class_fail(self):
        before={"kind":"StatefulSet","metadata":{"name":"eva-agent-qdrant"},"spec":{"replicas":1,
            "volumeClaimTemplates":[{"metadata":{"name":"storage"},"spec":{"storageClassName":"nks-block"}}],
            "template":{"spec":{"containers":[]}}}}
        after=copy.deepcopy(before)
        after["spec"]["volumeClaimTemplates"][0]["spec"]["storageClassName"]="new-block"
        with self.assertRaisesRegex(RuntimeError,"claim templates"):
            guard.validate({"component":"qdrant","namespace":"eva-agent"},[after],[before],lambda kind,namespace=None,name=None:[])
        with self.assertRaisesRegex(RuntimeError,"Unknown cluster StorageClass"):
            guard.validate({"component":"qdrant","namespace":"eva-agent"},[after],[],lambda kind,namespace=None,name=None:[])


if __name__=="__main__":
    unittest.main(verbosity=2)
