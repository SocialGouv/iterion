import asyncio, json, sys
import httpx2
from fastmcp import FastMCP, Client
spec = {"openapi":"3.0.3", "info":{"title":"Fixture API","version":"1"}, "paths":{"/items":{"get":{"operationId":"list_items", "parameters":[{"name":"timeout","in":"query","schema":{"type":"number","minimum":0,"exclusiveMinimum":True,"maximum":300,"exclusiveMaximum":False}},{"name":"q","in":"query","schema":{"type":"string","nullable":True}}],"responses":{"200":{"description":"OK"}}}}}}
async def main():
    async with httpx2.AsyncClient(base_url='http://fixture.invalid') as http:
        server=FastMCP.from_openapi(openapi_spec=spec, client=http)
        async with Client(server) as client:
            tools=await client.list_tools()
            with open(sys.argv[1], 'w') as f: json.dump(tools[0].inputSchema, f, indent=2); f.write('\n')
asyncio.run(main())
