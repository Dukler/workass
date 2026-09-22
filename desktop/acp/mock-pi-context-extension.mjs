// Deterministic Pi extension fixture. Never performs filesystem/shell/network
// work; the test model calls it to exercise real SDK transcript tool deltas.
export default function (pi) {
  const parameters = {type:'object',properties:{},additionalProperties:false};
  pi.registerTool({
    name:'fixture_removed',label:'Fixture removed tool',description:'Removed by the fixture during the turn.',parameters,
    async execute() { throw new Error('The removed fixture tool must never execute'); },
  });
  pi.registerTool({
    name:'fixture_swap',label:'Fixture tool selection',description:'Update the fixture tool selection.',parameters,
    async execute() {
      pi.setActiveTools([...new Set([
        ...pi.getActiveTools().filter(name => name !== 'bash' && name !== 'fixture_removed'),
        'powershell',
      ])]);
      return {content:[{type:'text',text:'fixture tool state changed'}],details:{}};
    },
  });
}
