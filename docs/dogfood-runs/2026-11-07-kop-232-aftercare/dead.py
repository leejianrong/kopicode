import ast,sys
src=open(sys.argv[1]).read()
try: tree=ast.parse(src)
except SyntaxError as e: print("syntax"); sys.exit(0)
dead=0
for n in ast.walk(tree):
    for fld in ("body","orelse","finalbody"):
        b=getattr(n,fld,None)
        if isinstance(b,list):
            for i,s in enumerate(b[:-1]):
                if isinstance(s,(ast.Return,ast.Raise)): dead+=1
print("dead" if dead else "clean")
