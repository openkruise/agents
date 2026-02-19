# Import and patch the E2B SDK
from e2b_code_interpreter import Sandbox
from kruise_agents.patch_e2b import patch_e2b
patch_e2b(False)

sbx: Sandbox = Sandbox.create(template="code-interpreter")
print(f"sandbox id: {sbx.sandbox_id}")
print(f"run code result: {sbx.run_code("print('hello, world')")}")
text = input("enter some text to save to file 'text.txt' in sandbox:  ")
sbx.files.write("text.txt", text)
print(f"read file from sandbox via filesystem api: [{sbx.files.read('text.txt')}]")
print(f"read file from sandbox via command: [{sbx.commands.run('cat text.txt')}]")
input("press ENTER to kill the sandbox")
print(sbx.kill())
